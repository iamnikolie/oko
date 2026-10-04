// Called via Runtime.callFunctionOn with `this` = scope element (or window).
// Returns {title,url,scroll,items:[...]} and registers every emitted
// interactive element in window.__okoRefs so later commands can address it by
// ref. A ref stays attached to its element across snapshots.
function (opts) {
  const top = window;
  if (!top.__okoRefs) {
    top.__okoRefs = new Map();
    top.__okoIds = new WeakMap();
    // Continue numbering from the last ref oko handed out in this tab, so a
    // ref from before a reload/navigation can never point at a new element.
    top.__okoSeq = opts.seqBase || 0;
  }
  const refOf = (el) => {
    let id = top.__okoIds.get(el);
    if (!id) {
      id = (opts.prefix || '') + 'e' + (++top.__okoSeq);
      top.__okoIds.set(el, id);
    }
    top.__okoRefs.set(id, el);
    return id;
  };

  const clean = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const trunc = (s, n) => (s.length > n ? s.slice(0, n - 1) + '…' : s);
  const MAXN = opts.maxName || 80;

  const LEAF = new Set(['button', 'link', 'checkbox', 'radio', 'switch', 'menuitem',
    'menuitemcheckbox', 'menuitemradio', 'option', 'tab', 'treeitem', 'slider',
    'spinbutton', 'textbox', 'searchbox', 'combobox', 'file', 'clickable',
    'color', 'date', 'datetime-local', 'month', 'time', 'week']);
  const INTERACTIVE = new Set([...LEAF, 'listbox']);
  const SKIP_TAGS = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'HEAD', 'META', 'LINK']);

  function roleOf(el) {
    const r = el.getAttribute('role');
    if (r && r !== 'presentation' && r !== 'none' && r !== 'generic') return r.split(/\s+/)[0];
    const t = el.tagName.toLowerCase();
    switch (t) {
      case 'a': return el.hasAttribute('href') ? 'link' : null;
      case 'button': return 'button';
      case 'input': {
        const ty = (el.getAttribute('type') || 'text').toLowerCase();
        if (ty === 'hidden') return null;
        if (['button', 'submit', 'reset', 'image'].includes(ty)) return 'button';
        if (ty === 'checkbox' || ty === 'radio') return ty;
        if (ty === 'range') return 'slider';
        if (ty === 'number') return 'spinbutton';
        if (ty === 'search') return 'searchbox';
        if (['file', 'color', 'date', 'datetime-local', 'month', 'time', 'week'].includes(ty)) return ty;
        return 'textbox';
      }
      case 'textarea': return 'textbox';
      case 'select': return el.multiple ? 'listbox' : 'combobox';
      case 'summary': return 'button';
      case 'h1': case 'h2': case 'h3': case 'h4': case 'h5': case 'h6': return 'heading';
      case 'dialog': return el.open ? 'dialog' : null;
      case 'label': return null;
    }
    if (el.isContentEditable && !(el.parentElement && el.parentElement.isContentEditable)) return 'textbox';
    return null;
  }

  function nameOf(el, role) {
    const al = clean(el.getAttribute('aria-label'));
    if (al) return al;
    const lb = el.getAttribute('aria-labelledby');
    if (lb) {
      const s = clean(lb.split(/\s+/).map((id) => {
        const e = el.ownerDocument.getElementById(id);
        return e ? (e.innerText || e.textContent) : '';
      }).join(' '));
      if (s) return s;
    }
    const t = el.tagName;
    if (t === 'INPUT' || t === 'TEXTAREA' || t === 'SELECT') {
      if (t === 'INPUT' && ['button', 'submit', 'reset'].includes(el.type)) return clean(el.value) || el.type;
      if (t === 'INPUT' && el.type === 'image') return clean(el.alt);
      if (el.labels && el.labels.length) {
        const s = clean(Array.from(el.labels).map((l) => l.innerText).join(' '));
        if (s) return s;
      }
      return clean(el.getAttribute('placeholder')) || clean(el.getAttribute('title')) || clean(el.getAttribute('name'));
    }
    if (role === 'textbox') return clean(el.getAttribute('placeholder')) || clean(el.getAttribute('data-placeholder')) || clean(el.getAttribute('title'));
    const txt = clean(el.innerText);
    if (txt) return txt;
    const img = el.querySelector('img[alt], [aria-label], [title]');
    if (img) return clean(img.getAttribute('alt') || img.getAttribute('aria-label') || img.getAttribute('title'));
    return clean(el.getAttribute('title'));
  }

  function attrsOf(el, role) {
    const a = {};
    const t = el.tagName;
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') a.disabled = true;
    if (role === 'checkbox' || role === 'radio' || role === 'switch' || role === 'menuitemcheckbox' || role === 'menuitemradio') {
      a.checked = (t === 'INPUT') ? el.checked : el.getAttribute('aria-checked') === 'true';
    }
    if (role === 'textbox' || role === 'searchbox' || role === 'spinbutton' || role === 'slider' ||
        ['date', 'datetime-local', 'month', 'time', 'week', 'color'].includes(role)) {
      let v = (t === 'INPUT' || t === 'TEXTAREA') ? el.value : (el.isContentEditable ? el.innerText : el.getAttribute('aria-valuenow'));
      v = clean(v || '');
      if (v && t === 'INPUT' && el.type === 'password') v = '•••';
      if (v) a.value = trunc(v, 60);
    }
    if (role === 'combobox' && t === 'SELECT') {
      const o = el.selectedOptions && el.selectedOptions[0];
      if (o) a.value = trunc(clean(o.text), 60);
    } else if (role === 'combobox' && t === 'INPUT' && clean(el.value)) {
      a.value = trunc(clean(el.value), 60);
    }
    if (role === 'file' && el.files && el.files.length) a.value = Array.from(el.files).map((f) => f.name).join(', ');
    const ex = el.getAttribute('aria-expanded');
    if (ex) a.expanded = ex === 'true';
    if (el.getAttribute('aria-selected') === 'true' || el.getAttribute('aria-current') && el.getAttribute('aria-current') !== 'false') a.selected = true;
    if (role === 'link') {
      const h = el.getAttribute('href') || '';
      if (h && !h.startsWith('javascript:') && h !== '#') {
        let s = h;
        try {
          const u = new URL(h, el.ownerDocument.baseURI);
          s = u.origin === location.origin ? u.pathname + u.search + u.hash : u.href;
        } catch (e) { /* keep raw */ }
        a.href = trunc(s, 70);
      }
    }
    if (el === el.ownerDocument.activeElement) a.focused = true;
    return a;
  }

  // State a person would notice: messages, invalid fields, loading.
  function signals() {
    const out = { messages: [], invalid: [], loading: 0 };
    const vis = (e) => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(e).visibility !== 'hidden'; };
    const seen = new Set();
    const msg = (t) => { t = clean(t); if (t && t.length < 300 && !seen.has(t)) { seen.add(t); out.messages.push(trunc(t, 160)); } };
    for (const e of document.querySelectorAll('[role=alert], [role=status], [aria-live=assertive], [aria-live=polite], [class*=toast i], [class*=snackbar i], [class*=notification i]')) {
      if (vis(e)) msg(e.innerText);
    }
    for (const t of (top.__okoLive || [])) msg(t);
    for (const e of document.querySelectorAll('[aria-invalid=true], :user-invalid')) {
      if (!vis(e)) continue;
      let err = '';
      const ids = (e.getAttribute('aria-errormessage') || '') + ' ' + (e.getAttribute('aria-describedby') || '');
      for (const id of ids.split(/\s+/).filter(Boolean)) { const d = document.getElementById(id); if (d) err += ' ' + d.innerText; }
      err = clean(err) || e.validationMessage || '';
      out.invalid.push({ ref: refOf(e), name: trunc(nameOf(e, roleOf(e) || 'textbox'), 60), error: trunc(clean(err), 120) });
    }
    for (const e of document.querySelectorAll('[aria-busy=true], [role=progressbar], [class*=spinner i], [class*=loading i], [class*=skeleton i]')) {
      if (vis(e)) out.loading++;
    }
    return out;
  }

  const vw = window.innerWidth, vh = window.innerHeight;
  function boxOf(el) {
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) return null;
    return r;
  }

  const items = [];
  const LIMIT = 5000;

  function visit(el, ctxPointer) {
    if (items.length >= LIMIT) return;
    if (SKIP_TAGS.has(el.tagName)) return;
    if (el.hidden || el.getAttribute('aria-hidden') === 'true' || el.inert) return;
    const st = getComputedStyle(el);
    if (st.display === 'none') return;
    const shown = st.visibility !== 'hidden' && st.visibility !== 'collapse';

    let role = roleOf(el);
    if (!role && shown && el.tagName !== 'LABEL' && el.tagName !== 'BODY' && el.tagName !== 'HTML') {
      const ti = el.getAttribute('tabindex');
      const pointer = st.cursor === 'pointer' && !ctxPointer;
      if (el.hasAttribute('onclick') || (ti !== null && +ti >= 0) || pointer) {
        if (clean(el.innerText) || el.getAttribute('aria-label') || el.getAttribute('title')) role = 'clickable';
      }
    }

    let descend = true, ownText = true;
    if (role && shown) {
      const box = boxOf(el);
      const inView = box && box.bottom > 0 && box.right > 0 && box.top < vh && box.left < vw;
      if (box && (!opts.viewport || inView)) {
        if (INTERACTIVE.has(role)) {
          items.push({ kind: 'el', el, ref: refOf(el), role, name: trunc(nameOf(el, role), MAXN), attrs: attrsOf(el, role), off: !inView });
          if (LEAF.has(role)) descend = false;
        } else if (role === 'heading') {
          const lvl = +(el.getAttribute('aria-level') || (el.tagName.match(/^H(\d)$/) || [])[1] || 2);
          const n = trunc(clean(el.innerText), 120);
          if (n) items.push({ kind: 'heading', level: lvl, name: n });
          descend = !!el.querySelector('a,button,input,select,textarea,[role],[tabindex]');
          ownText = false;
        } else if (role === 'dialog' || role === 'alertdialog' || role === 'alert' || (role === 'status' && opts.text)) {
          items.push({ kind: 'marker', role, name: trunc(nameOf(el, role), MAXN) });
        }
      }
    }
    if (!descend) return;

    const pointerCtx = ctxPointer || st.cursor === 'pointer';
    const kids = el.shadowRoot ? [...el.shadowRoot.children, ...el.children] : el.children;
    if (opts.text && shown) {
      // Collect this element's own text nodes, interleaved with children.
      for (const n of (el.shadowRoot ? kids : el.childNodes)) {
        if (n.nodeType === 3 && ownText) {
          const s = clean(n.textContent);
          if (s) {
            const last = items[items.length - 1];
            if (last && last.kind === 'text' && last.parent === el) last.name = trunc(last.name + ' ' + s, 300);
            else items.push({ kind: 'text', parent: el, name: trunc(s, 300) });
          }
        } else if (n.nodeType === 1) {
          visit(n, pointerCtx);
        }
      }
    } else {
      for (const k of kids) visit(k, pointerCtx);
    }
    if (el.tagName === 'IFRAME') {
      try {
        const d = el.contentDocument;
        if (d && d.body) {
          items.push({ kind: 'marker', role: 'iframe', name: trunc(el.getAttribute('title') || el.src || '', MAXN) });
          visit(d.body, false);
        }
      } catch (e) { /* cross-origin */ }
    }
  }

  let root = (this && this.nodeType === 1) ? this : document.body;
  // An open modal makes the rest of the page inert for the user; show only
  // the topmost one.
  let modal = null;
  if (root === document.body) {
    const cands = document.querySelectorAll('[aria-modal="true"], dialog[open], [role=dialog], [role=alertdialog]');
    for (const d of cands) {
      let isModal = d.getAttribute('aria-modal') === 'true';
      try { isModal = isModal || d.matches(':modal'); } catch (e) { /* old engines */ }
      if (!isModal) continue;
      const r = d.getBoundingClientRect();
      if (r.width > 0 && r.height > 0 && getComputedStyle(d).visibility !== 'hidden') modal = d;
    }
    if (modal) root = modal;
  }
  if (root) visit(root, false);

  // Disambiguate repeated names ("Delete" x 12) with the text of the row,
  // list item or card that holds each one.
  const count = new Map();
  for (const it of items) if (it.kind === 'el') {
    const k = it.role + '\u0000' + it.name;
    count.set(k, (count.get(k) || 0) + 1);
  }
  for (const it of items) if (it.kind === 'el' && count.get(it.role + '\u0000' + it.name) > 1) {
    const c = it.el.closest('tr,li,[role=row],[role=listitem],article,[role=article],fieldset,form,section');
    if (c) {
      let s = clean(c.innerText);
      if (it.name) s = clean(s.split(it.name).join(" "));
      if (s) it.ctx = trunc(s, 50);
    }
  }

  // Keep a light copy of a full-page snapshot so the next one can be diffed
  // against it (scoped or viewport-only snapshots would make false diffs).
  const light = items.map((it) => ({ kind: it.kind, ref: it.ref, role: it.role, name: it.name, level: it.level, attrs: it.attrs, ctx: it.ctx }));
  const prev = top.__okoPrev || null;
  const scope = modal ? 'dialog "' + trunc(nameOf(modal, 'dialog'), 60) + '"' : 'page';
  if (!opts.viewport && (root === document.body || modal)) top.__okoPrev = { scope, items: light };

  const se = document.scrollingElement || document.documentElement;
  return {
    prev: opts.diff ? prev : undefined,
    scope,
    signals: signals(),
    title: document.title,
    url: location.href,
    seq: top.__okoSeq,
    modal: modal ? true : undefined,
    scrollY: Math.round(window.scrollY),
    scrollH: Math.round(se.scrollHeight),
    vw, vh,
    items: items.map((it) => ({ kind: it.kind, ref: it.ref, role: it.role, name: it.name, level: it.level, attrs: it.attrs, ctx: it.ctx, off: it.off || undefined })),
  };
}
