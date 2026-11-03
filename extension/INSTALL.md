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

After an update of oko, reload the extension in `chrome://extensions` (the
circular arrow on its card). Extension 0.2 pairs differently from 0.1: run
`oko ext pair` once more and type the new code.

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
