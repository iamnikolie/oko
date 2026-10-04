package chrome

import _ "embed"

//go:embed snapshot.js
var SnapshotJS string

//go:embed read.js
var ReadJS string

// ResolveRefJS returns the element registered under a snapshot ref, or null
// when the ref is unknown or its element left the DOM.
const ResolveRefJS = `function (ref) {
  const m = window.__okoRefs;
  const el = m && m.get(ref);
  return el && el.isConnected ? el : null;
}`

// FindTextJS returns the best visible element whose text matches: interactive
// elements first, exact match before substring, smallest element wins.
const FindTextJS = `function (needle) {
  const want = needle.replace(/\s+/g, ' ').trim().toLowerCase();
  const sel = 'a,button,input,select,textarea,summary,label,[role],[tabindex],[onclick],h1,h2,h3,h4,h5,h6,li,td,th,p,span,div';
  let best = null, bestScore = -1;
  for (const el of document.querySelectorAll(sel)) {
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) continue;
    let t = (el.innerText || el.value || el.getAttribute('aria-label') || el.getAttribute('placeholder') || '').replace(/\s+/g, ' ').trim().toLowerCase();
    if (!t || !t.includes(want)) continue;
    const exact = t === want;
    const inter = el.matches('a,button,input,select,textarea,summary,[role=button],[role=link],[role=tab],[role=menuitem],[role=option],[role=checkbox]');
    const score = (exact ? 2000000 : 0) + (inter ? 1000000 : 0) - t.length;
    if (score > bestScore) { best = el; bestScore = score; }
  }
  return best;
}`

// SettleJS resolves once the document is loaded and the DOM has been quiet
// for quietMs (or maxMs passes), so actions report the page after the click
// rather than mid-transition.
const SettleJS = `function (quietMs, maxMs) {
  return new Promise((resolve) => {
    const start = Date.now();
    let last = Date.now();
    const mo = new MutationObserver(() => { last = Date.now(); });
    mo.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
    const tick = () => {
      const now = Date.now();
      if ((document.readyState === 'complete' && now - last >= quietMs) || now - start >= maxMs) {
        mo.disconnect();
        resolve(true);
      } else setTimeout(tick, 50);
    };
    tick();
  });
}`

// PerfEntriesJS lists the requests the page has made, from the Resource
// Timing buffer. It needs no prior attachment, so it sees history.
const PerfEntriesJS = `function () {
  const out = [];
  for (const e of performance.getEntriesByType('navigation').concat(performance.getEntriesByType('resource'))) {
    out.push({ url: e.name, type: e.initiatorType || 'navigation', status: e.responseStatus || 0,
      ms: Math.round(e.duration), size: e.transferSize || 0, start: Math.round(e.startTime) });
  }
  return out;
}`

// TextJS returns readable text of a scope element (or the body).
const TextJS = `function () {
  const el = (this && this.nodeType === 1) ? this : document.body;
  return (el.innerText || '').replace(/[ \t]+\n/g, '\n').replace(/\n{3,}/g, '\n\n').trim();
}`

// LiveWatchJS records text that appears in live regions, alerts and toasts
// from now on, so a message that flashes and disappears before an action
// settles is still reported.
const LiveWatchJS = `function () {
  window.__okoLive = [];
  if (window.__okoLiveObs) window.__okoLiveObs.disconnect();
  const sel = '[role=alert], [role=status], [aria-live], [class*=toast i], [class*=snackbar i], [class*=notification i]';
  const note = (n) => {
    const el = n.nodeType === 1 ? n : n.parentElement;
    if (!el) return;
    const host = el.matches(sel) ? el : el.closest(sel);
    if (!host) return;
    const t = (host.innerText || '').replace(/\s+/g, ' ').trim();
    if (t && !window.__okoLive.includes(t)) window.__okoLive.push(t);
  };
  const obs = new MutationObserver((ms) => {
    for (const m of ms) {
      if (m.type === 'characterData') note(m.target);
      for (const n of m.addedNodes) {
        note(n);
        if (n.nodeType === 1) for (const c of n.querySelectorAll(sel)) note(c);
      }
    }
  });
  obs.observe(document, { subtree: true, childList: true, characterData: true });
  window.__okoLiveObs = obs;
}`
