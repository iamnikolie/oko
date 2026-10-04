// The element picker behind 'oko watch'. Runs in the "oko" isolated world of
// every tab's top frame: it shares the DOM with the page but none of its
// JavaScript, so the page can neither see the overlay's state nor call the
// __okoPick binding that reports picks to the watcher.
//
// Alt+P toggles pick mode; click picks (with an optional note), Shift+click
// picks and stays in the mode, Alt+click picks without entering it, ArrowUp
// moves to the parent, Escape leaves. The watcher calls back into
// window.__okoPicker (done / spent / remove / setMode).
(() => {
  if (window.__okoPicker || window.top !== window) return;

  const BIND = '__okoPick';
  // The watcher installs the binding shortly after a document appears;
  // anything sent before that waits in the queue until flush().
  const queue = [];
  const send = (o) => {
    if (typeof window[BIND] === 'function') {
      try { window[BIND](JSON.stringify(o)); return; } catch (e) { /* fall through */ }
    }
    queue.push(o);
  };
  const clean = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const cut = (s, n) => (s.length > n ? s.slice(0, n - 1) + '…' : s);
  const circ = (n) => '①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳'.charAt(n - 1) || '(' + n + ')';

  const CSS = `
    :host { all: initial; }
    * { box-sizing: border-box; }
    .hover { position: fixed; border: 2px solid #e5433a; background: rgba(229,67,58,.12); border-radius: 3px; pointer-events: none; }
    .mark { position: fixed; border: 1.5px dashed #e5433a; border-radius: 3px; pointer-events: none; }
    .mark.spent { border-color: #8a909b; }
    .badge { position: fixed; min-width: 20px; height: 20px; padding: 0 5px; border-radius: 10px; display: flex; align-items: center; justify-content: center;
      font: 500 11.5px/1 ui-monospace, "SF Mono", Menlo, Consolas, monospace; background: #e5433a; color: #fff; box-shadow: 0 2px 6px rgba(0,0,0,.3); pointer-events: none; }
    .badge.spent { background: #8a909b; }
    .tip, .pop, .pill { font: 12px/1.45 ui-monospace, "SF Mono", Menlo, Consolas, monospace; color: #d7dae0; background: #13151a; }
    .tip { position: fixed; max-width: 360px; padding: 7px 10px; border-radius: 7px; box-shadow: 0 8px 24px -8px rgba(0,0,0,.5); pointer-events: none; overflow-wrap: anywhere; }
    .tip b, .pop b { color: #ff8f80; font-weight: 500; }
    .dim { color: #7d8590; }
    .pop { position: fixed; width: 300px; max-width: calc(100vw - 16px); padding: 10px; border-radius: 10px; box-shadow: 0 14px 34px -10px rgba(0,0,0,.6);
      display: flex; flex-direction: column; gap: 8px; pointer-events: auto; }
    .pop textarea { font: 13px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; resize: none; height: 64px; padding: 7px 9px; border-radius: 7px;
      border: 1px solid #2a2e37; background: #1c1f26; color: #d7dae0; outline: none; width: 100%; }
    .pop textarea:focus { border-color: #ff8f80; }
    .pop .foot { display: flex; justify-content: space-between; gap: 8px; flex-wrap: wrap; color: #7d8590; }
    .pill { position: fixed; top: 12px; right: 12px; padding: 5px 10px; border-radius: 999px; background: #e5433a; color: #fff;
      box-shadow: 0 4px 14px -4px rgba(0,0,0,.35); pointer-events: none; display: flex; align-items: center; gap: 6px; }
    .pill.warn { background: #13151a; color: #ff8f80; }
    .rec { width: 7px; height: 7px; border-radius: 50%; background: currentColor; }
    @media (prefers-reduced-motion: no-preference) { .rec { animation: blink 1.2s ease-in-out infinite; } }
    @keyframes blink { 50% { opacity: .3; } }
    [hidden] { display: none !important; }
  `;

  let host = null, root, layer, hoverBox, tip, pop, popHead, popTa, pill;
  function mount() {
    if (host) {
      if (!host.isConnected) (document.documentElement || document).appendChild(host);
      return;
    }
    host = document.createElement('oko-picker');
    host.style.cssText = 'all:initial;position:fixed;inset:0;z-index:2147483647;pointer-events:none;';
    root = host.attachShadow({ mode: 'closed' });
    const style = document.createElement('style');
    style.textContent = CSS;
    layer = document.createElement('div');
    hoverBox = el('div', 'hover');
    tip = el('div', 'tip');
    pop = el('div', 'pop');
    popHead = document.createElement('div');
    popTa = document.createElement('textarea');
    popTa.placeholder = "what's wrong with it? (optional)";
    const foot = document.createElement('div');
    foot.className = 'foot';
    foot.append(span('Enter save'), span('Shift+Enter newline · Esc cancel'));
    pop.append(popHead, popTa, foot);
    pill = el('div', 'pill');
    root.append(style, layer, hoverBox, tip, pop, pill);
    (document.documentElement || document).appendChild(host);

    // Keys typed into the note must not reach the page's shortcuts.
    for (const ev of ['keydown', 'keyup', 'keypress', 'input', 'beforeinput', 'paste', 'copy', 'cut']) {
      popTa.addEventListener(ev, (e) => e.stopPropagation());
    }
    popTa.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); commit(); }
      else if (e.key === 'Escape') { e.preventDefault(); closePop(); }
    });
  }
  function el(tag, cls) { const d = document.createElement(tag); d.className = cls; d.hidden = true; return d; }
  function span(t) { const s = document.createElement('span'); s.textContent = t; return s; }

  // ---- describing an element (cheap parts; the watcher adds ref, source, crop)

  function roleOf(e) {
    const r = e.getAttribute('role');
    if (r && r !== 'presentation' && r !== 'none' && r !== 'generic') return r.split(/\s+/)[0];
    const t = e.tagName.toLowerCase();
    switch (t) {
      case 'a': return e.hasAttribute('href') ? 'link' : 'text';
      case 'button': case 'summary': return 'button';
      case 'input': {
        const ty = (e.getAttribute('type') || 'text').toLowerCase();
        if (['button', 'submit', 'reset', 'image'].includes(ty)) return 'button';
        if (ty === 'checkbox' || ty === 'radio') return ty;
        return 'textbox';
      }
      case 'textarea': return 'textbox';
      case 'select': return 'combobox';
      case 'img': case 'svg': return 'img';
      case 'h1': case 'h2': case 'h3': case 'h4': case 'h5': case 'h6': return 'heading';
      case 'nav': return 'navigation';
      case 'header': return 'banner';
      case 'footer': return 'contentinfo';
      case 'main': return 'main';
      case 'table': return 'table';
      case 'tr': return 'row';
      case 'td': return 'cell';
      case 'th': return 'columnheader';
      case 'li': return 'listitem';
      case 'ul': case 'ol': return 'list';
      case 'form': return 'form';
      case 'dialog': return 'dialog';
    }
    return e.children.length ? 'group' : 'text';
  }
  function nameOf(e) {
    const s = e.getAttribute('aria-label') || e.getAttribute('alt') || e.getAttribute('title') ||
      (('value' in e && typeof e.value === 'string' && e.value) ? e.value : '') || e.getAttribute('placeholder') || e.innerText || e.textContent;
    return cut(clean(s), 60);
  }
  // The row or card the element sits in, as text without its controls
  // (like the snapshot's "in:"), so "Details" says which order it belongs to.
  function ctxOf(e) {
    const row = e.parentElement && e.parentElement.closest('tr,li,[role=row],[role=listitem],article');
    if (!row) return '';
    const parts = [];
    const walk = document.createTreeWalker(row, NodeFilter.SHOW_TEXT, {
      acceptNode: (t) => (t.parentElement && t.parentElement.closest('button,a,input,select,textarea,[role=button],script,style,svg') && !t.parentElement.closest('button,a,input,select,textarea,[role=button],script,style,svg').contains(row))
        ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT,
    });
    for (let t = walk.nextNode(); t && parts.length < 30; t = walk.nextNode()) {
      const s = clean(t.data);
      if (s) parts.push(s);
    }
    return cut(parts.join(' '), 90);
  }
  function selOf(e) {
    const parts = [];
    let n = e;
    while (n && n.nodeType === 1 && n !== document.documentElement && parts.length < 4) {
      if (n.id && /^[A-Za-z][\w-]*$/.test(n.id)) { parts.unshift('#' + n.id); break; }
      const cls = Array.from(n.classList).filter((c) => /^[A-Za-z_-][\w-]*$/.test(c) && c.length < 32).slice(0, 2);
      parts.unshift(n.tagName.toLowerCase() + (cls.length ? '.' + cls.join('.') : ''));
      n = n.parentElement;
    }
    return parts.join(' > ');
  }
  function hex(rgb) {
    const m = (rgb || '').match(/[\d.]+/g);
    if (!m || m.length < 3 || (m.length === 4 && +m[3] === 0)) return '';
    return '#' + m.slice(0, 3).map((v) => Math.round(+v).toString(16).padStart(2, '0')).join('');
  }
  function describe(e) {
    const cs = getComputedStyle(e);
    const r = e.getBoundingClientRect();
    const pad = [cs.paddingTop, cs.paddingRight, cs.paddingBottom, cs.paddingLeft];
    const padStr = pad.every((p) => p === '0px') ? '' : (pad[0] === pad[2] && pad[1] === pad[3] ? (pad[0] === pad[1] ? pad[0] : pad[0] + ' ' + pad[1]) : pad.join(' '));
    const lh = parseFloat(cs.lineHeight);
    return {
      role: roleOf(e), name: nameOf(e), ctx: ctxOf(e), sel: selOf(e),
      w: Math.round(r.width), h: Math.round(r.height),
      x: Math.round(r.left + scrollX), y: Math.round(r.top + scrollY),
      pad: padStr, bg: hex(cs.backgroundColor), color: hex(cs.color),
      font: Math.round(parseFloat(cs.fontSize)) + '/' + (isNaN(lh) ? 'normal' : Math.round(lh)) + ' ' +
        cs.fontFamily.split(',')[0].replace(/["']/g, '').trim() + ' ' + cs.fontWeight,
    };
  }

  // Component + source location live in the page's own JavaScript (React
  // fibers, Vue instances), invisible from this world. Once armed, the watcher
  // installs a small listener in the main world that answers an 'oko-src'
  // event by writing the answer to an attribute we read back synchronously.
  let armed = false;
  const srcCache = new WeakMap();
  function srcOf(e) {
    if (!armed) return null;
    if (srcCache.has(e)) return srcCache.get(e);
    const de = document.documentElement;
    let v = null;
    try {
      e.dispatchEvent(new CustomEvent('oko-src', { bubbles: true, composed: true }));
      v = de.getAttribute('data-oko-src');
      de.removeAttribute('data-oko-src');
    } catch (err) { /* ignore */ }
    if (v === null) return null; // helper not installed yet; ask again later
    const [comp, file] = v.split('|');
    const res = comp || file ? { comp, file } : { comp: '', file: '' };
    srcCache.set(e, res);
    return res;
  }
  function arm() { if (!armed) { armed = true; send({ t: 'arm' }); } }

  // ---- state

  let mode = false, hoverEl = null, pending = null, keep = false;
  let n = 0, inMode = 0, warnUntil = 0;
  const marks = []; // {id, el, n, state: 'wait' | 'unread' | 'spent'}

  function setMode(on) {
    if (on === mode) return;
    mount();
    mode = on;
    if (on) { arm(); inMode = 0; } else { hoverEl = null; }
    send({ t: 'mode', on, picked: inMode });
    kick();
  }

  function deepTarget(ev) {
    const t = ev.composedPath ? ev.composedPath()[0] : ev.target;
    if (!t || t.nodeType !== 1 || t === host) return null;
    if (t === document.documentElement || t === document.body) return null;
    return t;
  }

  function openPop(t) {
    mount();
    pending = { el: t };
    hoverEl = t;
    popHead.textContent = '';
    const b = document.createElement('b');
    b.textContent = circ(n + 1) + ' ' + roleOf(t);
    popHead.append(b, document.createTextNode(' "' + nameOf(t) + '"'));
    popTa.value = '';
    pop.hidden = false;
    render();
    popTa.focus({ preventScroll: true });
  }
  function closePop() {
    pop.hidden = true;
    pending = null;
    kick();
  }
  function commit() {
    const t = pending.el;
    const id = Math.random().toString(16).slice(2, 10);
    const d = describe(t);
    const m = { id, el: t, n: ++n, state: 'wait' };
    marks.push(m);
    inMode++;
    t.setAttribute('data-oko-pick', id);
    const payload = Object.assign({ t: 'pick', id, n: m.n, note: clean(popTa.value), url: location.href, title: document.title }, d);
    closePop();
    if (!keep) setMode(false);
    // Hide the overlay so the watcher's crop shows the page, not our marks.
    host.style.visibility = 'hidden';
    requestAnimationFrame(() => requestAnimationFrame(() => {
      send(payload);
      // No answer: the watcher is gone (or very slow). Say so instead of
      // pretending the pick reached the agent.
      setTimeout(() => {
        if (m.state !== 'wait') return;
        host.style.visibility = '';
        if (queue.includes(payload)) {
          queue.splice(queue.indexOf(payload), 1);
          t.removeAttribute('data-oko-pick');
          marks.splice(marks.indexOf(m), 1);
          warnUntil = Date.now() + 4000;
        } else m.state = 'unread';
        kick();
      }, 3000);
    }));
  }

  // ---- drawing

  let raf = 0;
  function kick() { if (!raf) raf = requestAnimationFrame(frame); }
  function frame() {
    raf = 0;
    render();
    if (mode || pending || marks.length || Date.now() < warnUntil) raf = requestAnimationFrame(frame);
  }
  function place(box, r) {
    box.style.left = r.left + 'px'; box.style.top = r.top + 'px';
    box.style.width = r.width + 'px'; box.style.height = r.height + 'px';
  }
  function onScreen(r) {
    return r.width > 0 && r.height > 0 && r.bottom > 0 && r.right > 0 && r.top < innerHeight && r.left < innerWidth;
  }
  function render() {
    if (!host) return;
    mount();
    const cur = pending ? pending.el : (mode ? hoverEl : null);
    if (cur && cur.isConnected) {
      const r = cur.getBoundingClientRect();
      place(hoverBox, r);
      hoverBox.hidden = false;
      if (!pending) drawTip(cur, r); else tip.hidden = true;
    } else { hoverBox.hidden = true; tip.hidden = true; }
    if (pending) placeNear(pop, pending.el.getBoundingClientRect(), 8);

    layer.textContent = '';
    for (let i = marks.length - 1; i >= 0; i--) if (!marks[i].el.isConnected) marks.splice(i, 1);
    for (const m of marks) {
      const r = m.el.getBoundingClientRect();
      if (!onScreen(r)) continue;
      const spent = m.state === 'spent' ? ' spent' : '';
      const box = document.createElement('div');
      box.className = 'mark' + spent;
      place(box, r);
      const b = document.createElement('div');
      b.className = 'badge' + spent;
      b.textContent = m.n;
      b.style.left = Math.max(0, r.left - 10) + 'px';
      b.style.top = Math.max(0, r.top - 10) + 'px';
      layer.append(box, b);
    }

    const unread = marks.filter((m) => m.state !== 'spent').length;
    pill.className = 'pill';
    pill.textContent = '';
    if (Date.now() < warnUntil) {
      pill.className = 'pill warn';
      pill.textContent = 'oko watch is not running; pick not saved';
      pill.hidden = false;
    } else if (mode) {
      const dot = document.createElement('span'); dot.className = 'rec';
      pill.append(dot, document.createTextNode('pick · Esc'));
      pill.hidden = false;
    } else if (unread) {
      pill.textContent = unread + (unread === 1 ? ' pick' : ' picks') + ' → agent';
      pill.hidden = false;
    } else pill.hidden = true;
  }
  function drawTip(e, r) {
    tip.textContent = '';
    const b = document.createElement('b');
    b.textContent = roleOf(e);
    const dim = (t) => { const s = document.createElement('span'); s.className = 'dim'; s.textContent = t; return s; };
    tip.append(b, document.createTextNode(' "' + nameOf(e) + '" '), dim(Math.round(r.width) + '×' + Math.round(r.height)));
    const src = srcOf(e);
    if (src && (src.comp || src.file)) {
      tip.append(document.createElement('br'), document.createTextNode(src.comp || '?'), dim(src.file ? ' · ' + src.file : ''));
    }
    tip.append(document.createElement('br'), dim('click pick · ↑ parent · Esc'));
    tip.hidden = false;
    placeNear(tip, r, 6);
  }
  function placeNear(box, r, gap) {
    const h = box.offsetHeight, w = box.offsetWidth;
    let top = r.bottom + gap;
    if (top + h > innerHeight - 8) top = Math.max(8, r.top - h - gap);
    const left = Math.min(Math.max(8, r.left), innerWidth - w - 8);
    box.style.top = top + 'px'; box.style.left = left + 'px';
  }

  // ---- input (capture phase on window: we see events before the page)

  addEventListener('keydown', (e) => {
    if (e.altKey && !e.ctrlKey && !e.metaKey && e.code === 'KeyP') {
      e.preventDefault(); e.stopImmediatePropagation();
      if (pending) closePop();
      setMode(!mode);
      return;
    }
    if (e.key === 'Escape' && mode && !pending) {
      e.preventDefault(); e.stopImmediatePropagation();
      setMode(false);
      return;
    }
    if (e.key === 'ArrowUp' && mode && hoverEl && !pending) {
      const p = hoverEl.parentElement;
      if (p && p !== document.body && p !== document.documentElement) {
        e.preventDefault(); e.stopImmediatePropagation();
        hoverEl = p;
        kick();
      }
    }
  }, true);

  const fromUs = (e) => host && e.composedPath && e.composedPath().includes(host);
  for (const ev of ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click', 'dblclick', 'auxclick', 'contextmenu']) {
    addEventListener(ev, (e) => {
      if (fromUs(e)) return;
      const altPick = e.altKey && e.button === 0 && !e.ctrlKey && !e.metaKey;
      if (!(mode || pending || altPick)) return;
      e.preventDefault(); e.stopImmediatePropagation();
      if (ev !== 'click' || pending) return;
      const t = (mode && hoverEl) || deepTarget(e);
      if (!t) return;
      arm();
      keep = mode && e.shiftKey;
      openPop(t);
    }, true);
  }
  addEventListener('mousemove', (e) => {
    if (!mode || pending) return;
    const t = deepTarget(e);
    if (t && t !== hoverEl) { hoverEl = t; kick(); }
  }, true);
  addEventListener('scroll', () => { if (host) kick(); }, true);
  addEventListener('resize', () => { if (host) kick(); });

  // ---- called by the watcher

  const byId = (id) => marks.find((m) => m.id === id);
  window.__okoPicker = {
    setMode: (on) => { setMode(!!on); return true; },
    flush: () => { for (const o of queue.splice(0)) send(o); return true; },
    done: (id) => {
      const m = byId(id);
      if (m && m.state === 'wait') m.state = 'unread';
      if (!marks.some((x) => x.state === 'wait') && host) host.style.visibility = '';
      kick();
      return true;
    },
    spent: (ids) => { for (const id of ids) { const m = byId(id); if (m) m.state = 'spent'; } kick(); return true; },
    remove: (ids) => {
      for (const id of ids) { const m = byId(id); if (m) marks.splice(marks.indexOf(m), 1); }
      kick();
      return true;
    },
  };
})();
