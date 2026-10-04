# oko

Browser CLI for coding agents. `oko` drives a dedicated Chrome over the
DevTools Protocol: every command attaches, does one thing, prints a compact
result and exits, so there is no extension or long-lived MCP connection to
lose. The browser keeps running between commands and keeps its logins.

```sh
oko open localhost:5173
oko read                 # the page's substance as markdown (no nav, no buttons)
oko snap                 # [e7] textbox "Email"  [e12] button "Save" …
oko fill e7 me@example.com
oko click e12 --snap
oko console --errors
oko net --failed
oko shot                 # prints a PNG path
oko record start / stop -o demo.gif
oko perf                 # Web Vitals; perf trace | heap | lighthouse
```

After every action oko prints signals (messages, invalid fields, loading) and
what changed (`+`/`-`/`~`), so the agent rarely needs another snapshot.
Cross-origin iframes get refs like `f1e3`. `trim` and `revive` handle long
feeds and crashed tabs; `--human` gives curved mouse paths and wheel scrolling.
`oko up --proxy http://user:pass@host:port` routes a profile through an
authenticated proxy (http, https, socks5) via a local relay.

**Point instead of describe.** In the oko window press Alt+P and click
elements (with an optional note); the agent gets each one as a snapshot ref,
component and source file (React/Vue/Svelte dev builds), CSS path, box and
styles, a crop and your note. `oko pick` asks you to point and waits; `oko
picks` reads what you picked. See [Picks in Claude Code and Codex](#picks-in-claude-code-and-codex)
to have picks arrive with your next message by themselves.

`oko skill` prints the full agent reference (also installed as a Claude Code /
Codex skill).

## Tests

```sh
make e2e                 # real binary vs headless Chrome and fixture pages (~20 s)
```

## Install

**Homebrew:**

```sh
brew trust iamnikolie/tap   # Homebrew 6 refuses untrusted third-party taps
brew tap iamnikolie/tap
brew install iamnikolie/tap/oko
```

**Prebuilt binary** from [Releases](https://github.com/iamnikolie/oko/releases)
(macOS and Linux, amd64/arm64), or **from source**:

```sh
go install github.com/iamnikolie/oko@latest
# or, in a checkout:
make install             # builds and links ~/.local/bin/oko
```

**Agent skill:** `oko skill` prints a ready `SKILL.md`:

```sh
mkdir -p ~/.claude/skills/oko && oko skill > ~/.claude/skills/oko/SKILL.md
mkdir -p ~/.codex/skills/oko && oko skill > ~/.codex/skills/oko/SKILL.md
```

macOS and Linux. Needs Google Chrome (or Chromium; set `OKO_CHROME` to the binary). State lives
in `~/.oko` (`OKO_HOME` to move it): one Chrome user-data dir per profile,
screenshots in `~/.oko/shots`.

## Picks in Claude Code and Codex

`oko picks --hook` is a `UserPromptSubmit` hook: each time you send a message,
the agent runs it, and whatever you picked in the browser since your last
message is added to that message's context (picks made in a tab another agent
session works in go to that session). With nothing picked it prints nothing
and returns in ~15 ms; it only reads files under `~/.oko`, never starts Chrome,
and never fails your prompt.

**Claude Code.** Add it to your user settings, `~/.claude/settings.json` (or
`$CLAUDE_CONFIG_DIR/settings.json`), for every project, or to a project's
`.claude/settings.json` for that project only. Merge it into an existing
`hooks` object rather than replacing it:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "oko picks --hook", "timeout": 5 } ] }
    ]
  }
}
```

**Codex.** Codex reads the same hook format from `~/.codex/hooks.json` (all
projects) or `<repo>/.codex/hooks.json` (one project); put the JSON above
there, merged with any hooks you have. Codex runs a new or changed hook only
after you trust it: open `/hooks` in Codex and trust `oko picks --hook`. The
same in `~/.codex/config.toml` instead of `hooks.json`:

```toml
[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "oko picks --hook"
timeout = 5
```

Use the absolute path (`which oko`) if `oko` is not on the `PATH` the agent
starts with. Check it: open `/hooks` (both agents list hooks there), press
Alt+P in the oko window, click something, and send any message; the agent's
context now holds an `<oko-picks>` block. Picks reach the session that works in
the tab: oko keys sessions by `$CLAUDE_CODE_SESSION_ID` or `$CODEX_THREAD_ID`,
which match the `session_id` the hook receives.

Without the hook (other agents, or by choice) the agent reads picks with `oko
picks`, waits for them with `oko picks --wait`, or asks you with `oko pick`.

The picker itself starts with the browser. `oko watch off` turns it off for a
profile, `oko watch on` back on.

## Design

- Own profile per `--profile`, launched with a random debugging port bound to
  127.0.0.1. oko finds it through the profile's `DevToolsActivePort` file and
  checks it, so it never attaches to some other Chrome.
- Current tab is per caller session (`--session` / `$OKO_SESSION`, else
  `$CLAUDE_CODE_SESSION_ID` or `$CODEX_THREAD_ID`, else a shared `default`), stored in
  `profiles/<name>/sessions/`. Agents sharing a profile get their own tabs;
  navigating or closing another session's tab needs `--force`.
- Snapshots are built in the page: interactive elements get refs (`e12`) kept
  in a page-side map, stable across snapshots while the element lives.
- Console history comes from Chrome's own replay on `Runtime.enable`, so
  errors logged before oko attached are visible.
- A proxied profile points Chrome at a local relay (`oko _proxy`, exits with
  Chrome) because Chrome takes no proxy credentials on its command line. The
  upstream URL, password included, sits in the profile's `state.json` (0600).
- The element picker runs from a background `oko _watch` per profile (exits
  with Chrome). It lives in an isolated world of every tab, so the page sees
  neither its overlay state nor the binding it reports through, and a page
  cannot forge picks. The watcher avoids `Runtime.enable` (a common bot-check
  signal) and reinstalls the binding for each new document instead. Picks are
  stored in `profiles/<name>/picks/`.
- JS dialogs are answered during every command (they would otherwise freeze
  the tab) and reported.

Security: anything local can drive a Chrome with an open debugging port. Keep
banking and primary accounts out of oko profiles.

## License

MIT
