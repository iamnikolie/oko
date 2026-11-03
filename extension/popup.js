import { restricted } from './restricted.js';

const $ = (id) => document.getElementById(id);
let current = null;
let forcePair = false;
let shownEye = '';
const favicons = new Map(); // tabId -> favIconUrl

async function call(msg) {
  const r = await chrome.runtime.sendMessage(msg);
  if (!r || !r.ok) throw new Error(r ? r.error : 'no answer from the extension');
  return r.state;
}

function showError(e) {
  $('error').textContent = e ? String(e.message || e) : '';
  $('error').hidden = !e;
}

// link -> [label, class, eye]
const LINK = {
  on: ['connected', 'ok', 'on'], connecting: ['connecting…', '', 'off'], off: ['bridge not running', 'bad', 'off'],
  refused: ['pairing refused', 'bad', 'half'], unpaired: ['not paired', 'bad', 'off'], untrusted: ['link closed', 'bad', 'half'],
};

function setEye(name) {
  const eye = $('eye');
  eye.firstElementChild.setAttribute('href', '#eye-' + name);
  if (shownEye && shownEye !== name) {
    eye.classList.remove('blink');
    void eye.getBoundingClientRect(); // restart the animation
    eye.classList.add('blink');
  }
  shownEye = name;
}

function host(url) {
  try {
    const u = new URL(url);
    return u.protocol === 'file:' ? u.pathname : u.host + (u.pathname === '/' ? '' : u.pathname);
  } catch {
    return url || '';
  }
}

// A 16 px favicon, or the title's first letter on a grey tile.
function fav(el, url, title) {
  const letter = () => {
    const s = document.createElement('span');
    s.className = 'fav';
    s.textContent = (title || '?').trim().charAt(0);
    return s;
  };
  let node = letter();
  if (url && /^(https?|data):/.test(url)) {
    const img = document.createElement('img');
    img.className = 'fav';
    img.alt = '';
    img.src = url;
    img.onerror = () => img.replaceWith(letter());
    node = img;
  }
  if (el) el.replaceWith(node);
  return node;
}

function render(s) {
  const [label, cls, eye] = LINK[s.link] || [s.link, '', 'off'];
  const mine = new Set(s.tabs.map((t) => t.tabId));
  const curHeld = !!current && mine.has(current.id);
  $('link-label').textContent = label;
  $('link').className = 'state ' + cls;
  setEye(s.link === 'on' && curHeld ? 'held' : eye);

  const untrusted = s.link === 'untrusted';
  $('untrusted').hidden = !untrusted;
  if (s.link === 'refused' && s.linkError) showError(s.linkError);
  $('untrusted-held').hidden = !(untrusted && s.tabs.length);
  const dropped = untrusted ? (s.linkError || '').replace(/^bridge not trusted:\s*/, '') : '';
  $('untrusted-why').textContent = dropped;
  $('untrusted-why').hidden = !dropped;
  const pairing = forcePair || !s.paired || s.link === 'refused' || untrusted;
  $('pairing').hidden = !pairing;
  $('pair-lead').hidden = untrusted;
  // While pairing, only tabs oko still holds stay listed, so they can be taken back.
  $('cur-sec').hidden = pairing;
  $('held-sec').hidden = pairing && s.tabs.length === 0;
  $('new-sec').hidden = pairing;
  $('repair').hidden = pairing;
  if (document.activeElement !== $('port')) $('port').value = s.port;
  $('allow-new').checked = !!s.allowNew;

  if (current) {
    const title = current.title || current.url || '(untitled)';
    $('cur-title').textContent = title;
    $('cur-host').textContent = host(current.url || current.pendingUrl);
    fav($('cur-fav'), current.favIconUrl, title).id = 'cur-fav';
    const why = restricted(current.url || current.pendingUrl || '');
    $('cur').classList.toggle('held', curHeld);
    $('cur-held').hidden = !curHeld;
    $('give').hidden = curHeld || !!why;
    $('release-cur').hidden = !curHeld;
    const note = why || (curHeld ? '' : s.released[current.id] ? 'oko no longer holds this tab: ' + s.released[current.id] + '.' : '');
    $('cur-note').textContent = note;
    $('cur-note').hidden = !note;
  }

  const ul = $('tabs');
  ul.textContent = '';
  for (const t of s.tabs) {
    const li = document.createElement('li');
    li.append(fav(null, favicons.get(t.tabId), t.title || t.url));
    const name = document.createElement('span');
    name.className = 't';
    name.textContent = t.title || t.url;
    name.title = t.url;
    li.append(name);
    if (t.own) {
      const own = document.createElement('span');
      own.className = 'own';
      own.textContent = 'opened by oko';
      li.append(own);
    }
    const b = document.createElement('button');
    b.className = 'text';
    b.textContent = 'Take back';
    b.onclick = () => act({ cmd: 'release', tabId: t.tabId });
    li.append(b);
    ul.append(li);
  }
  $('count').textContent = s.tabs.length ? String(s.tabs.length) : '';
  $('none').hidden = s.tabs.length > 0;
  $('release-all').hidden = s.tabs.length === 0;
  $('foot').textContent = s.tabs.length && !pairing
    ? 'While oko holds a tab, Chrome shows a “started debugging this browser” bar. Cancel there takes the tab back.'
    : 'oko only reaches the tabs you give it here.';
}

async function act(msg) {
  showError(null);
  try {
    render(await call(msg));
  } catch (e) {
    showError(e);
    try { render(await call({ cmd: 'state' })); } catch { /* */ }
  }
}

$('give').onclick = () => act({ cmd: 'give', tabId: current.id });
$('release-cur').onclick = () => act({ cmd: 'release', tabId: current.id });
$('release-all').onclick = () => act({ cmd: 'releaseAll' });
$('allow-new').onchange = (e) => act({ cmd: 'allowNew', on: e.target.checked });
$('repair').onclick = () => { forcePair = true; act({ cmd: 'state' }); };
$('pair').onclick = async () => {
  forcePair = false;
  await act({ cmd: 'pair', code: $('code').value.trim(), port: $('port').value });
  $('code').value = '';
};
$('code').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('pair').click(); });

(async () => {
  for (const t of await chrome.tabs.query({})) if (t.favIconUrl) favicons.set(t.id, t.favIconUrl);
  [current] = await chrome.tabs.query({ active: true, currentWindow: true });
  await act({ cmd: 'connect' });
})();
