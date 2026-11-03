// oko extension service worker: the bridge between the oko CLI and the tabs
// the user hands over. It holds one chrome.debugger attachment per handed-over
// tab and relays CDP for it to the local oko bridge (ws://127.0.0.1:<port>/ext).
// The bridge must prove it holds the pairing key before the worker sends it
// anything or runs any of its commands, every frame after that is
// authenticated both ways (protocol.js), and only the CDP methods in
// cdp-policy.json reach a tab.
import { restricted } from './restricted.js';
import policy from './cdp-policy.json' with { type: 'json' };
import {
  hex, unhex, randomHex, validNonce, hmac, hmacHex, same, codeKey,
  pairTranscript, helloBridgeMsg, helloExtMsg, ecdhKeys, ecdhShared, channel,
} from './protocol.js';

const CLIENT_METHODS = new Set(policy.client);
const BRIDGE_METHODS = new Set(policy.bridge);

const DEFAULT_PORT = 47222;
const PROTOCOL = '1.3';

// tabId -> { own: bool }. Mirrored to storage.session so a restarted worker
// knows which attachments are its own; cleared when Chrome restarts.
const controlled = new Map();
const lastRelease = new Map(); // tabId -> reason, shown in the popup
const children = new Map(); // tabId -> Set of child session ids (frames)
let loaded = null;

let ws = null; // the socket being set up or in use
let trustedSock = null; // set once the bridge proved itself on it
let trustedCh = null; // its frame channel
let sendChain = Promise.resolve(); // frames go out in counter order
let link = 'off'; // off | connecting | on | refused | unpaired | untrusted
let linkError = '';
let pingTimer = null;
let pairWaiter = null;
let retryMs = 1000;

const info = () => ({
  version: chrome.runtime.getManifest().version,
  product: 'Chrome/' + ((navigator.userAgent.match(/Chrome\/([\d.]+)/) || [])[1] || '?'),
  userAgent: navigator.userAgent,
});

async function settings() {
  return chrome.storage.local.get({ key: '', port: DEFAULT_PORT, allowNew: false });
}

async function load() {
  if (!loaded) {
    loaded = (async () => {
      const { tabs = {} } = await chrome.storage.session.get('tabs');
      const targets = await chrome.debugger.getTargets();
      for (const [id, v] of Object.entries(tabs)) {
        const tabId = Number(id);
        const t = targets.find((x) => x.tabId === tabId);
        // Keep only attachments that survived; never re-attach silently.
        if (t && t.attached) controlled.set(tabId, v);
      }
      await save();
      for (const tabId of controlled.keys()) paintTab(tabId);
    })();
  }
  return loaded;
}

async function save() {
  await chrome.storage.session.set({ tabs: Object.fromEntries(controlled) });
}

// The toolbar eye is the link state: open = connected; half-lidded with a red
// "!" = bridge not trusted or pairing refused; closed = off or not paired. A tab
// oko holds shows the filled eye while the link is on. A tab-specific icon
// cannot be unset, so every tab that ever got one is repainted with the link.
const EYE = { on: 'on', refused: 'half', untrusted: 'half' }; // else 'off'
const TITLE = {
  on: 'oko', connecting: 'oko: connecting', off: 'oko: bridge not running',
  unpaired: 'oko: not paired', refused: 'oko: pairing refused', untrusted: 'oko: link closed',
};
const DANGER = '#c8322f';
const eyeIcon = (eye) => ({ 16: `icons/${eye}-16.png`, 32: `icons/${eye}-32.png`, 48: `icons/${eye}-48.png` });
const painted = new Set(); // tabs with a tab-specific icon and title
let paintedLink = '';

function paint() {
  if (link === paintedLink) return;
  paintedLink = link;
  const eye = EYE[link] || 'off';
  chrome.action.setIcon({ path: eyeIcon(eye) }).catch(() => {});
  chrome.action.setBadgeText({ text: eye === 'half' ? '!' : '' }).catch(() => {});
  chrome.action.setBadgeBackgroundColor({ color: DANGER }).catch(() => {});
  chrome.action.setTitle({ title: TITLE[link] || 'oko' }).catch(() => {});
  for (const tabId of painted) paintTab(tabId);
}

function paintTab(tabId) {
  painted.add(tabId);
  const t = controlled.get(tabId);
  if (t && link === 'on') {
    chrome.action.setIcon({ tabId, path: eyeIcon('held') }).catch(() => {});
    chrome.action.setTitle({ tabId, title: t.own ? 'oko opened and controls this tab' : 'oko controls this tab' }).catch(() => {});
  } else {
    chrome.action.setIcon({ tabId, path: eyeIcon(EYE[link] || 'off') }).catch(() => {});
    chrome.action.setTitle({ tabId, title: TITLE[link] || 'oko' }).catch(() => {});
  }
}

function attachError(e) {
  const m = String(e && e.message || e);
  if (/already attached/i.test(m)) {
    return new Error('DevTools or another debugger is open on this tab. Close DevTools and try again.');
  }
  if (/file/i.test(m) && /access/i.test(m)) {
    return new Error('file:// pages need "Allow access to file URLs" on the oko extension (chrome://extensions).');
  }
  if (/cannot (access|attach)/i.test(m)) {
    return new Error('Chrome does not let extensions drive this page.');
  }
  return new Error(m);
}

export async function giveTab(tabId, own = false) {
  await load();
  if (controlled.has(tabId)) return;
  const tab = await chrome.tabs.get(tabId);
  const why = restricted(tab.url || tab.pendingUrl || '');
  if (why) throw new Error(why);
  try {
    await chrome.debugger.attach({ tabId }, PROTOCOL);
  } catch (e) {
    throw attachError(e);
  }
  controlled.set(tabId, { own });
  lastRelease.delete(tabId);
  await save();
  paintTab(tabId);
  pushTabs();
}

export async function releaseTab(tabId, reason = 'taken back in the popup') {
  await load();
  if (!controlled.has(tabId)) return;
  controlled.delete(tabId);
  children.delete(tabId);
  lastRelease.set(tabId, reason);
  // Detach first: a failed storage write must not leave the debugger on a tab
  // oko no longer tracks. A stale stored entry is harmless, since load() keeps
  // only tabs that are still attached.
  try { await chrome.debugger.detach({ tabId }); } catch { /* already gone */ }
  paintTab(tabId);
  pushTabs();
  try { await save(); } catch { /* see above */ }
}

// releaseAll lets go of every tab, each on its own: one failure does not keep
// the rest held.
export async function releaseAll(reason = 'taken back with "Take all back"') {
  await load();
  await Promise.allSettled([...controlled.keys()].map((tabId) => releaseTab(tabId, reason)));
}

export async function setAllowNew(on) {
  await chrome.storage.local.set({ allowNew: !!on });
  pushTabs();
}

export async function setPort(port) {
  await chrome.storage.local.set({ port: Number(port) || DEFAULT_PORT });
}

async function targetIds() {
  const map = new Map();
  for (const t of await chrome.debugger.getTargets()) if (t.tabId !== undefined) map.set(t.tabId, t.id);
  return map;
}

async function tabList() {
  await load();
  const ids = await targetIds();
  const out = [];
  for (const [tabId, v] of controlled) {
    try {
      const t = await chrome.tabs.get(tabId);
      out.push({ tabId, targetId: ids.get(tabId) || '', url: t.url || t.pendingUrl || '', title: t.title || '', own: !!v.own });
    } catch { /* closing */ }
  }
  return out;
}

let pushTimer = null;
function pushTabs() {
  clearTimeout(pushTimer);
  pushTimer = setTimeout(async () => {
    if (link !== 'on') return;
    const { allowNew } = await settings();
    send({ ev: 'tabs', tabs: await tabList(), allowNew });
  }, 50);
}

function send(msg) {
  const sock = trustedSock;
  if (!sock || sock.readyState !== WebSocket.OPEN) return;
  const frame = trustedCh.seal(JSON.stringify(msg));
  sendChain = sendChain.then(() => frame).then((f) => {
    if (sock.readyState === WebSocket.OPEN) sock.send(f);
  }).catch(() => { /* socket gone */ });
}

async function connect() {
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;
  const { key, port } = await settings();
  if (!key) {
    link = 'unpaired';
    paint();
    return;
  }
  open(port, 'hello');
}

// A closed link lets go of every tab: after a failed security check oko holds
// nothing until the user gives a tab again.
const DROP_REASON = 'the link failed a security check';
let dropped = 0; // tabs the last closed link let go of
let drops = 0; // closed links so far; a tab op started before one must not hold a tab after it

async function dropAll() {
  drops++;
  await load();
  dropped = controlled.size;
  await releaseAll(DROP_REASON);
}

// distrust drops a socket whose peer failed to prove itself; nothing was sent
// to it but nonces and public keys.
function distrust(sock, reason) {
  if (ws === sock) ws = null;
  link = 'untrusted';
  paint();
  linkError = reason;
  try { sock.close(); } catch { /* */ }
  pairWaiter?.reject(new Error(reason));
  pairWaiter = null;
  dropAll().catch(() => { /* tabs already gone */ });
}

function trust(sock, ch) {
  trustedSock = sock;
  trustedCh = ch;
  link = 'on';
  paint();
  linkError = '';
  retryMs = 1000;
  clearInterval(pingTimer);
  // Websocket traffic keeps the service worker alive.
  pingTimer = setInterval(() => send({ type: 'ping' }), 20000);
  pushTabs();
  pairWaiter?.resolve();
  pairWaiter = null;
}

// handshake handles one message before the bridge is trusted.
async function handshake(sock, st, m) {
  const untrusted = (why) => distrust(sock, 'bridge not trusted: ' + why);
  switch (m.type) {
    case 'challenge': {
      if (st.mode !== 'hello' || st.proved || !validNonce(m.nonce)) return untrusted('unexpected challenge');
      const { key } = await settings();
      if (!same(await hmacHex(unhex(key), helloBridgeMsg(st.nonce, m.nonce)), m.proof)) {
        return untrusted('it could not prove the pairing key (another program on the port, or oko was paired again elsewhere)');
      }
      st.proved = true;
      st.ch = await channel(unhex(key), st.nonce, m.nonce);
      sock.send(JSON.stringify({ type: 'auth', proof: await hmacHex(unhex(key), helloExtMsg(st.nonce, m.nonce)) }));
      return;
    }
    case 'welcome':
      if (st.mode !== 'hello' || !st.proved) return untrusted('it skipped the proof');
      return trust(sock, st.ch);
    case 'pair-challenge': {
      if (st.mode !== 'pair' || st.proved || !validNonce(m.nonce) || !/^[0-9a-f]{130}$/.test(m.pub || '')) {
        return untrusted('unexpected pairing answer');
      }
      const tr = pairTranscript(st.pub, m.pub, st.nonce, m.nonce);
      const ck = await codeKey(st.code, m.nonce);
      if (!same(await hmacHex(ck, 'bridge|' + tr), m.proof)) {
        return untrusted('it did not prove the pairing code (check the code; is \'oko ext pair\' the one on this port?)');
      }
      const shared = await ecdhShared(st.kp, m.pub);
      st.key = hex(await hmac(shared, 'oko-key|' + tr));
      st.ch = await channel(unhex(st.key), st.nonce, m.nonce);
      st.proved = true;
      sock.send(JSON.stringify({ type: 'pair-confirm', proof: await hmacHex(ck, 'ext|' + tr) }));
      return;
    }
    case 'paired':
      if (st.mode !== 'pair' || !st.proved) return untrusted('it skipped the proof');
      await chrome.storage.local.set({ key: st.key });
      await chrome.storage.local.remove('token');
      return trust(sock, st.ch);
    case 'refused':
      if (ws === sock) ws = null;
      link = 'refused';
      paint();
      linkError = m.reason || 'refused';
      try { sock.close(); } catch { /* */ }
      pairWaiter?.reject(new Error(linkError));
      pairWaiter = null;
      return;
  }
  return untrusted('unexpected message before authentication');
}

function open(port, mode, code = '') {
  link = 'connecting';
  paint();
  const sock = new WebSocket(`ws://127.0.0.1:${port}/ext`);
  ws = sock;
  const st = { mode, code, nonce: randomHex(16), proved: false };
  let queue = Promise.resolve();
  sock.onopen = async () => {
    if (mode === 'pair') {
      const { kp, pub } = await ecdhKeys();
      st.kp = kp;
      st.pub = pub;
      sock.send(JSON.stringify({ type: 'pair', pub, nonce: st.nonce, ...info() }));
    } else {
      sock.send(JSON.stringify({ type: 'hello', nonce: st.nonce, ...info() }));
    }
  };
  sock.onmessage = (ev) => {
    // Handle messages one at a time, in order.
    queue = queue.then(async () => {
      if (ws !== sock && trustedSock !== sock) return;
      let m;
      if (trustedSock !== sock) {
        try { m = JSON.parse(ev.data); } catch { return; }
        try { await handshake(sock, st, m); } catch (e) { distrust(sock, 'bridge not trusted: ' + (e.message || e)); }
        return;
      }
      // A forged, altered, replayed or reordered frame ends the session.
      try {
        m = JSON.parse(await st.ch.open(ev.data));
      } catch (e) {
        trustedSock = null;
        clearInterval(pingTimer);
        return distrust(sock, 'bridge not trusted: ' + (e.message || e));
      }
      if (m.type === 'refused') {
        link = 'refused';
        paint();
        linkError = m.reason || 'refused';
        try { sock.close(); } catch { /* */ }
        return;
      }
      if (m.op) reply(m);
    });
  };
  sock.onclose = () => {
    if (trustedSock === sock) {
      trustedSock = null;
      clearInterval(pingTimer);
    }
    // A socket replaced by a newer one (re-pairing) has nothing to say.
    if (ws !== sock) return;
    ws = null;
    clearInterval(pingTimer);
    if (link === 'refused' || link === 'untrusted') return;
    link = 'off';
    paint();
    if (pairWaiter) pairWaiter.reject(new Error(`no oko bridge on 127.0.0.1:${port}: run 'oko ext pair' first`));
    pairWaiter = null;
    // While a tab is held the worker stays alive: come back quickly when
    // the bridge restarts. Otherwise the 30 s alarm reconnects.
    if (controlled.size > 0) {
      setTimeout(connect, retryMs);
      retryMs = Math.min(retryMs * 2, 5000);
    }
  };
}

export async function pair(code, port) {
  if (port) await setPort(port);
  const s = await settings();
  if (ws) { try { ws.close(); } catch { /* */ } ws = null; }
  if (trustedSock) { try { trustedSock.close(); } catch { /* */ } trustedSock = null; }
  const done = new Promise((resolve, reject) => { pairWaiter = { resolve, reject }; });
  open(s.port, 'pair', code);
  return done;
}

function cdpError(e) {
  const m = String(e && e.message || e);
  try {
    const j = JSON.parse(m);
    if (j && j.message) return { code: j.code || -32000, message: j.message };
  } catch { /* plain text */ }
  return { code: -32000, message: m };
}

async function reply(m) {
  try {
    send({ id: m.id, result: await op(m) ?? {} });
  } catch (e) {
    send({ id: m.id, error: cdpError(e) });
  }
}

// denied explains why the bridge may not run method on a tab, or returns ''.
// The bridge applies the same policy; this is the extension's own check.
function denied(tabId, sessionId, method, params) {
  if (sessionId && !children.get(tabId)?.has(sessionId)) return 'not a frame of this tab';
  if (BRIDGE_METHODS.has(method)) {
    if (method === 'Target.setAutoAttach') {
      const keys = Object.keys(params).sort().join(',');
      if (keys !== 'autoAttach,flatten,waitForDebuggerOnStart' || params.autoAttach !== true || params.flatten !== true || params.waitForDebuggerOnStart !== false) {
        return 'Target.setAutoAttach: only plain frame auto-attach';
      }
    }
    return '';
  }
  if (!CLIENT_METHODS.has(method)) return method + ': not allowed through the oko extension';
  if (method === 'Page.navigate') {
    const url = String(params.url || '');
    if (restricted(url)) return restricted(url);
    if (url !== 'about:blank' && !/^https?:\/\//i.test(url)) return 'only http(s) and about:blank can be opened through the extension';
  }
  return '';
}

async function op(m) {
  await load();
  const mine = (tabId) => {
    if (!controlled.has(tabId)) throw new Error('tab is not handed to oko (any more)');
  };
  switch (m.op) {
    case 'cdp': {
      mine(m.tabId);
      const why = denied(m.tabId, m.sessionId, m.method, m.params || {});
      if (why) throw new Error(why);
      const target = m.sessionId ? { tabId: m.tabId, sessionId: m.sessionId } : { tabId: m.tabId };
      return chrome.debugger.sendCommand(target, m.method, m.params || {});
    }
    case 'createTab': {
      const gen = drops;
      const { allowNew } = await settings();
      if (!allowNew) throw new Error('oko may not open tabs: enable it in the oko popup');
      const url = m.url || 'about:blank';
      const why = restricted(url);
      if (why) throw new Error(why);
      let tab;
      if (m.newWindow) {
        const win = await chrome.windows.create({ url, focused: false, type: 'normal' });
        tab = win.tabs[0];
      } else {
        tab = await chrome.tabs.create({ url, active: !m.background });
      }
      try {
        const group = await chrome.tabs.group({ tabIds: [tab.id], createProperties: { windowId: tab.windowId } });
        await chrome.tabGroups.update(group, { title: 'oko', color: 'orange' });
      } catch { /* groups are cosmetic */ }
      await giveTab(tab.id, true);
      if (drops !== gen) {
        // The link closed while the tab was opening: oko holds nothing now.
        await releaseTab(tab.id, DROP_REASON);
        throw new Error('the link closed while the tab was opening');
      }
      const t = (await tabList()).find((x) => x.tabId === tab.id);
      return t;
    }
    case 'closeTab':
      mine(m.tabId);
      await chrome.tabs.remove(m.tabId);
      return {};
    case 'activateTab': {
      mine(m.tabId);
      const t = await chrome.tabs.update(m.tabId, { active: true });
      await chrome.windows.update(t.windowId, { focused: true });
      return {};
    }
    case 'releaseAll':
      await releaseAll('released by oko');
      return {};
  }
  throw new Error('unknown op ' + m.op);
}

export async function state() {
  await load();
  const s = await settings();
  return {
    link, linkError, paired: !!s.key, port: s.port, allowNew: s.allowNew,
    tabs: await tabList(),
    released: Object.fromEntries(lastRelease), dropped,
  };
}

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (source.tabId === undefined || !controlled.has(source.tabId)) return;
  if (!source.sessionId && method === 'Target.attachedToTarget' && params?.sessionId) {
    if (!children.has(source.tabId)) children.set(source.tabId, new Set());
    children.get(source.tabId).add(params.sessionId);
  }
  if (!source.sessionId && method === 'Target.detachedFromTarget' && params?.sessionId) {
    children.get(source.tabId)?.delete(params.sessionId);
  }
  send({ ev: 'cdp', tabId: source.tabId, sessionId: source.sessionId || '', method, params: params || {} });
});

chrome.debugger.onDetach.addListener((source, reason) => {
  const tabId = source.tabId;
  if (tabId === undefined || !controlled.has(tabId)) return;
  controlled.delete(tabId);
  children.delete(tabId);
  lastRelease.set(tabId, reason === 'canceled_by_user' ? 'debugging cancelled in Chrome’s bar' : reason);
  save();
  paintTab(tabId);
  pushTabs();
});

chrome.tabs.onRemoved.addListener((tabId) => {
  painted.delete(tabId);
  if (!controlled.has(tabId)) return;
  controlled.delete(tabId);
  children.delete(tabId);
  save();
  pushTabs();
});

chrome.tabs.onUpdated.addListener((tabId, change) => {
  if (!controlled.has(tabId)) return;
  if (change.url && restricted(change.url)) {
    releaseTab(tabId, 'navigated to a page oko may not drive');
    return;
  }
  if (change.url || change.title || change.status === 'complete') {
    paintTab(tabId);
    pushTabs();
  }
});

chrome.runtime.onMessage.addListener((msg, _sender, respond) => {
  (async () => {
    switch (msg.cmd) {
      case 'state': return state();
      case 'give': await giveTab(msg.tabId); return state();
      case 'release': await releaseTab(msg.tabId); return state();
      case 'releaseAll': await releaseAll(); return state();
      case 'allowNew': await setAllowNew(msg.on); return state();
      case 'pair': await pair(msg.code, msg.port); return state();
      case 'connect': await connect(); return state();
    }
    throw new Error('unknown command');
  })().then((v) => respond({ ok: true, state: v }), (e) => respond({ ok: false, error: String(e.message || e) }));
  return true;
});

paint();
chrome.alarms.create('oko-connect', { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener((a) => { if (a.name === 'oko-connect') connect(); });
chrome.runtime.onStartup.addListener(() => connect());
chrome.runtime.onInstalled.addListener(() => connect());

// For the e2e harness (a throwaway Chrome evaluates in this worker).
globalThis.oko = { giveTab, releaseTab, releaseAll, setAllowNew, setPort, pair, state, connect, _op: op, _channel: channel, _dropAll: dropAll };

load().then(connect);
