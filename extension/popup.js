import { restricted } from './restricted.js';

const $ = (id) => document.getElementById(id);
let current = null;
let forcePair = false;

async function call(msg) {
  const r = await chrome.runtime.sendMessage(msg);
  if (!r || !r.ok) throw new Error(r ? r.error : 'no answer from the extension');
  return r.state;
}

function showError(e) {
  $('error').textContent = e ? String(e.message || e) : '';
  $('error').hidden = !e;
}

const LINK = {
  on: ['connected', 'on'], connecting: ['connecting…', ''], off: ['oko bridge not running', 'bad'],
  refused: ['refused', 'bad'], unpaired: ['not paired', 'bad'],
};

function render(s) {
  const [label, cls] = LINK[s.link] || [s.link, ''];
  $('link').textContent = label;
  $('link').className = 'pill ' + cls;
  if (s.link === 'refused' && s.linkError) showError(s.linkError);
  $('pairing').hidden = !(forcePair || !s.paired || s.link === 'refused');
  $('port').value = s.port;
  $('allow-new').checked = !!s.allowNew;

  const mine = new Set(s.tabs.map((t) => t.tabId));
  if (current) {
    $('cur-title').textContent = current.title || current.url || '(untitled)';
    const why = restricted(current.url || current.pendingUrl || '');
    const has = mine.has(current.id);
    $('give').hidden = has || !!why;
    $('release-cur').hidden = !has;
    const note = why || (has ? '' : s.released[current.id] ? 'Released: ' + s.released[current.id] + '.' : '');
    $('cur-note').textContent = note;
    $('cur-note').hidden = !note;
  }

  const ul = $('tabs');
  ul.textContent = '';
  for (const t of s.tabs) {
    const li = document.createElement('li');
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
    b.textContent = 'Release';
    b.onclick = () => act({ cmd: 'release', tabId: t.tabId });
    li.append(b);
    ul.append(li);
  }
  $('count').textContent = s.tabs.length ? String(s.tabs.length) : '';
  $('none').hidden = s.tabs.length > 0;
  $('release-all').hidden = s.tabs.length === 0;
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
  [current] = await chrome.tabs.query({ active: true, currentWindow: true });
  await act({ cmd: 'connect' });
})();
