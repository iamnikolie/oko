// oko extension service worker: the bridge between the oko CLI and the tabs
// the user hands over. It holds one chrome.debugger attachment per handed-over
// tab and relays CDP for it to the local oko bridge (ws://127.0.0.1:<port>/ext),
// authenticated with the token from the one-time pairing.
import { restricted } from './restricted.js';

const DEFAULT_PORT = 47222;
const PROTOCOL = '1.3';
const BADGE = '#c2410c';

// tabId -> { own: bool }. Mirrored to storage.session so a restarted worker
// knows which attachments are its own; cleared when Chrome restarts.
const controlled = new Map();
const lastRelease = new Map(); // tabId -> reason, shown in the popup
let loaded = null;

let ws = null;
let link = 'off'; // off | connecting | on | refused | unpaired
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
  return chrome.storage.local.get({ token: '', port: DEFAULT_PORT, allowNew: false });
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
      for (const tabId of controlled.keys()) badge(tabId);
    })();
  }
  return loaded;
}

async function save() {
  await chrome.storage.session.set({ tabs: Object.fromEntries(controlled) });
}

function badge(tabId) {
  const own = controlled.get(tabId)?.own;
  chrome.action.setBadgeText({ tabId, text: own ? 'OKO+' : 'OKO' }).catch(() => {});
  chrome.action.setBadgeBackgroundColor({ tabId, color: BADGE }).catch(() => {});
  chrome.action.setTitle({ tabId, title: own ? 'oko opened and controls this tab' : 'oko controls this tab' }).catch(() => {});
}

function unbadge(tabId) {
  chrome.action.setBadgeText({ tabId, text: '' }).catch(() => {});
  chrome.action.setTitle({ tabId, title: 'oko' }).catch(() => {});
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
  badge(tabId);
  pushTabs();
}

export async function releaseTab(tabId, reason = 'released in the popup') {
  await load();
  if (!controlled.has(tabId)) return;
  controlled.delete(tabId);
  lastRelease.set(tabId, reason);
  await save();
  try { await chrome.debugger.detach({ tabId }); } catch { /* already gone */ }
  unbadge(tabId);
  pushTabs();
}

export async function releaseAll(reason = 'released with "Disconnect all"') {
  await load();
  for (const tabId of [...controlled.keys()]) await releaseTab(tabId, reason);
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
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
}

async function connect() {
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;
  const { token, port } = await settings();
  if (!token) {
    link = 'unpaired';
    return;
  }
  open(port, { type: 'hello', token, ...info() });
}

function open(port, first) {
  link = 'connecting';
  const sock = new WebSocket(`ws://127.0.0.1:${port}/ext`);
  ws = sock;
  sock.onopen = () => sock.send(JSON.stringify(first));
  sock.onmessage = async (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.type === 'welcome' || m.type === 'paired') {
      if (m.type === 'paired') await chrome.storage.local.set({ token: m.token });
      link = 'on';
      linkError = '';
      retryMs = 1000;
      clearInterval(pingTimer);
      // Websocket traffic keeps the service worker alive.
      pingTimer = setInterval(() => send({ type: 'ping' }), 20000);
      pushTabs();
      pairWaiter?.resolve();
      pairWaiter = null;
      return;
    }
    if (m.type === 'refused') {
      if (ws !== sock) return;
      link = 'refused';
      linkError = m.reason || 'refused';
      if (ws === sock) ws = null;
      try { sock.close(); } catch { /* */ }
      pairWaiter?.reject(new Error(linkError));
      pairWaiter = null;
      return;
    }
    if (m.op) reply(m);
  };
  sock.onclose = () => {
    // A socket replaced by a newer one (re-pairing) has nothing to say.
    if (ws !== sock) return;
    ws = null;
    clearInterval(pingTimer);
    if (link !== 'refused') {
      link = 'off';
      // While a tab is held the worker stays alive: come back quickly when
      // the bridge restarts. Otherwise the 30 s alarm reconnects.
      if (!pairWaiter && controlled.size > 0) {
        setTimeout(connect, retryMs);
        retryMs = Math.min(retryMs * 2, 5000);
      }
      if (pairWaiter) pairWaiter.reject(new Error(`no oko bridge on 127.0.0.1:${port}: run 'oko ext pair' first`));
      pairWaiter = null;
    }
  };
}

export async function pair(code, port) {
  if (port) await setPort(port);
  const s = await settings();
  if (ws) { try { ws.close(); } catch { /* */ } ws = null; }
  const done = new Promise((resolve, reject) => { pairWaiter = { resolve, reject }; });
  open(s.port, { type: 'pair', code, ...info() });
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

async function op(m) {
  await load();
  const mine = (tabId) => {
    if (!controlled.has(tabId)) throw new Error('tab is not handed to oko (any more)');
  };
  switch (m.op) {
    case 'cdp': {
      mine(m.tabId);
      const target = m.sessionId ? { tabId: m.tabId, sessionId: m.sessionId } : { tabId: m.tabId };
      return chrome.debugger.sendCommand(target, m.method, m.params || {});
    }
    case 'createTab': {
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
    link, linkError, paired: !!s.token, port: s.port, allowNew: s.allowNew,
    tabs: await tabList(),
    released: Object.fromEntries(lastRelease),
  };
}

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (source.tabId === undefined || !controlled.has(source.tabId)) return;
  send({ ev: 'cdp', tabId: source.tabId, sessionId: source.sessionId || '', method, params: params || {} });
});

chrome.debugger.onDetach.addListener((source, reason) => {
  const tabId = source.tabId;
  if (tabId === undefined || !controlled.has(tabId)) return;
  controlled.delete(tabId);
  lastRelease.set(tabId, reason === 'canceled_by_user' ? 'debugging cancelled in Chrome’s bar' : reason);
  save();
  unbadge(tabId);
  pushTabs();
});

chrome.tabs.onRemoved.addListener((tabId) => {
  if (!controlled.has(tabId)) return;
  controlled.delete(tabId);
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
    badge(tabId);
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

chrome.alarms.create('oko-connect', { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener((a) => { if (a.name === 'oko-connect') connect(); });
chrome.runtime.onStartup.addListener(() => connect());
chrome.runtime.onInstalled.addListener(() => connect());

// For the e2e harness (a throwaway Chrome evaluates in this worker).
globalThis.oko = { giveTab, releaseTab, releaseAll, setAllowNew, setPort, pair, state, connect };

load().then(connect);
