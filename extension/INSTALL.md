# oko extension — install, use, build, remove

Lets oko drive tabs of your everyday Chrome. Local only: the extension is not
in the Chrome Web Store, it ships inside the oko binary, and it talks only to
`127.0.0.1`.

## Install and pair (once)

1. Install oko (Homebrew, a release binary or `make install`; see the
   [README](../README.md#install)). Chrome 125 or newer.
2. Run `oko ext pair`. It writes the extension to
   `~/.oko/extension/unpacked`, starts the bridge, prints a code (valid 10
   minutes, works once) and waits.
3. Chrome → `chrome://extensions` → turn on **Developer mode** → **Load unpacked**
   → pick `~/.oko/extension/unpacked` (in the macOS file dialog press
   Cmd+Shift+. to see the hidden `.oko` folder, or Cmd+Shift+G and paste the
   path). Pin the oko eye from the puzzle-piece menu.
4. Click the oko eye, type the code, press **Pair**. `oko ext pair` prints
   `paired:` and exits; the eye opens.

## Use

- On a tab oko may drive: oko icon → **Give this tab to oko**. On that tab the
  oko eye turns solid orange, and Chrome shows its yellow "oko started debugging
  this browser" bar — Chrome always shows it for extension debuggers and it
  cannot be hidden.
- The toolbar eye is the link state: open with an orange pupil = connected;
  closed = the bridge is not running or the extension is not paired;
  half-lidded red with a `!` = the link closed because the bridge failed a
  security check, or pairing was refused. A closed link lets go of every tab;
  the extension reconnects by itself, then give the tabs again. If it keeps
  happening, pair again with a fresh code.
- Then any command with `-p mine`: `oko -p mine snap`, `oko -p mine click e12`, …
- oko sees only the tabs you gave it. To let it open tabs of its own, turn on
  **Let oko open its own tabs** in the popup; those tabs land in an orange
  "oko" group and are marked "opened by oko" in the popup.
- Not possible: Chrome's own pages (`chrome://…`), the Web Store, other
  extensions' pages. `file://` pages need **Allow access to file URLs** on the
  extension's details page.
- If a tab refuses because DevTools is open on it, close DevTools and give it again.

## Take tabs back

- One tab: popup → **Take back** next to it (or **Take it back** on the tab
  itself), or **Cancel** on Chrome's yellow bar.
- Everything: popup → **Take all back**, or `oko ext release`.
- `oko -p mine down` (or `oko ext stop`) releases every tab and stops the bridge.

## Roll back

1. `oko ext unpair` (the bridge forgets the extension's token).
2. `chrome://extensions` → oko → **Remove**.
3. Optional: `rm -rf ~/.oko/extension ~/.oko/profiles/mine`.

## Updating

1. Update oko (`brew upgrade oko`, a new release binary, or `make install`).
2. `oko ext path` writes the new extension files over
   `~/.oko/extension/unpacked`.
3. `chrome://extensions` → the oko card → the circular reload arrow. Do not
   skip it: Chrome reads the popup from disk each time it opens, but keeps the
   old service worker and toolbar icon until the reload, so the two halves
   disagree. The card shows the new version once it took.
4. Only when the pairing protocol changed (0.1 → 0.2): the popup says "pairing
   refused"; run `oko ext pair` and type the new code.

## Build and develop

The extension is plain JavaScript with no build step (MV3, ES modules):
`manifest.json`, `background.js` (service worker: pairing, the authenticated
link to the bridge, `chrome.debugger` per handed-over tab, toolbar icon),
`protocol.js` (HMAC/ECDH/channel), `cdp-policy.json` (the CDP allow-list, also
read by the bridge), `popup.*`, `restricted.js`, `icons/`. `embed.go` embeds
these files in the oko binary; the bridge side is `internal/bridge`, the CLI
side `cmd/ext.go`.

```sh
make build                    # go build: the binary carries the extension
./oko ext path                # write it to ~/.oko/extension/unpacked
# chrome://extensions → reload the oko card (or Load unpacked the first time)
make lint test                # go vet + unit tests
go test ./e2e -run '^TestExtension' -count=1 -v   # real Chrome, see below
```

A new file under `extension/` must also be listed in the `go:embed` line of
`embed.go`. A new CDP method a command needs goes into `cdp-policy.json`
(`client` for oko commands, `bridge` for the bridge's own calls), never as a
wildcard. Bump `version` in `manifest.json` when the extension's behaviour
changes; `chrome://extensions` and `oko ext status` show it.

The e2e tests load the unpacked extension into a throwaway headless Chrome
with a temporary profile (`--use-mock-keychain --password-store=basic`, so
the macOS keychain is never touched), pair it with a bridge on a free port and
drive it: real commands on handed-over tabs, the CDP boundary, impersonation
and relayed-frame attacks, release paths. `$OKO_EXT_CHROME` picks another
Chrome binary.

To look at the popup without Chrome APIs, open `popup.html` over a local HTTP
server with a stub `chrome` object (`chrome.runtime.sendMessage` resolving to
`{ ok: true, state }` and `chrome.tabs.query`); the shape of `state` is
`state()` in `background.js`.

## How it is guarded

- Both sides prove themselves on every connection, the bridge first: an HMAC
  challenge-response with a key both derived during pairing (ECDH + the
  one-time code). The key never crosses the socket. Something else listening
  on the port gets only random nonces, and the popup shows "link
  closed".
- After that every message, both ways, carries a counter and an HMAC under a
  key for that connection and direction. A program that relays a genuine
  handshake cannot add, change, replay or reorder a message: the first one
  that does not check out ends the connection.
- When the connection ends on a failed check, the extension also detaches from
  every tab it held: oko holds nothing until you give a tab again.
- Pairing: the code (10 minutes, one use, 5 attempts) is never sent either;
  each side proves it with an HMAC keyed by PBKDF2(code).
- The bridge listens on `127.0.0.1` only (`$OKO_EXT_PORT`, default 47222) and
  answers only requests addressed to `127.0.0.1`/`localhost`; requests with a
  browser `Origin` are refused on the oko-command side.
- oko commands prove they hold `~/.oko/extension/client.secret` (0600) the same
  way, after the bridge proved it first, and get a short-lived ticket; the
  secret itself is never sent.
- Only the CDP methods page commands need reach a tab (`cdp-policy.json`,
  checked by the bridge and again by the extension): no cookies, no storage,
  no other tabs, no browser-level calls, no downloads; navigation only to
  http(s) and about:blank.
