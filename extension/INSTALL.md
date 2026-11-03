# oko extension — install, use, remove

Lets oko drive tabs of your everyday Chrome. Local only: nothing is published,
the extension talks only to `127.0.0.1`.

## Install and pair (once)

1. Build/install oko from this checkout: `make install`.
2. Run `oko ext pair`. It prints a code (valid 10 minutes, works once) and a
   folder, `~/.oko/extension/unpacked`.
3. Chrome → `chrome://extensions` → turn on **Developer mode** → **Load unpacked**
   → pick that folder. Pin the oko icon if you like.
4. Click the oko icon, type the code, press **Pair**. `oko ext pair` prints
   `paired:` and exits.

## Use

- On a tab oko may drive: oko icon → **Give this tab to oko**. The icon shows
  `OKO` on that tab, and Chrome shows its yellow "oko started debugging this
  browser" bar — Chrome always shows it for extension debuggers and it cannot be
  hidden.
- Then any command with `-p mine`: `oko -p mine snap`, `oko -p mine click e12`, …
- oko sees only the tabs you gave it. To let it open tabs of its own, tick
  **Allow oko to open its own tabs** in the popup; those tabs land in an orange
  "oko" group and show `OKO+`.
- Not possible: Chrome's own pages (`chrome://…`), the Web Store, other
  extensions' pages. `file://` pages need **Allow access to file URLs** on the
  extension's details page.
- If a tab refuses because DevTools is open on it, close DevTools and give it again.

## Release

- One tab: popup → **Release** (or **Release this tab**), or **Cancel** on
  Chrome's yellow bar.
- Everything: popup → **Disconnect all**, or `oko ext release`.
- `oko -p mine down` (or `oko ext stop`) releases every tab and stops the bridge.

## Roll back

1. `oko ext unpair` (the bridge forgets the extension's token).
2. `chrome://extensions` → oko → **Remove**.
3. Optional: `rm -rf ~/.oko/extension ~/.oko/profiles/mine`.

## How it is guarded

- The bridge listens on `127.0.0.1` only (`$OKO_EXT_PORT`, default 47222) and
  answers only requests addressed to `127.0.0.1`/`localhost`.
- The extension is admitted only with the token from pairing and from the
  extension origin it paired from; only the token's hash is on disk.
- oko commands present a secret from `~/.oko/extension/client.secret` (0600);
  requests carrying a browser `Origin` are refused, so web pages cannot reach it.
- Browser-level CDP (windows, downloads, other tabs) is not relayed at all.
