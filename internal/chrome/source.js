// okoSource finds the component and source location behind a DOM element,
// from what dev builds leave on the page. Runs in the page's main world.
// Returns {comp, file} (either may be empty) or null.
function okoSource(el) {
  const strip = (u) => {
    if (!u) return '';
    if (/^webpack[\w-]*:/.test(u)) return u.replace(/^webpack[\w-]*:\/*\.?\/?/, '').replace(/[?#].*$/, '');
    // Absolute file paths (React's _debugSource) are kept as they are.
    if (!/^[a-z][\w+.-]*:\/\//i.test(u)) return u;
    try { const x = new URL(u); u = x.origin === location.origin ? x.pathname : x.href; } catch (e) { /* not a URL */ }
    return u.replace(/[?#].*$/, '').replace(/^\/@fs(?=\/)/, '').replace(/^\/(?!Users\/|home\/)/, '');
  };
  const nameOf = (t) => (t && (t.displayName || t.name || (t.render && (t.render.displayName || t.render.name)) || (t.type && nameOf(t.type)))) || '';
  // React 19 dropped _debugSource; its dev elements carry the creation stack.
  const fromStack = (st) => {
    if (!st) return '';
    const s = typeof st === 'string' ? st : st.stack || '';
    for (const line of s.split('\n').slice(1)) {
      if (/react-stack-top-frame|jsxDEV|jsx-dev-runtime|node_modules|react-dom|\/react\//.test(line)) continue;
      const m = line.match(/((?:https?|file|webpack[\w-]*):\/\/[^\s)]+?|\/[^\s)]+?):(\d+):\d+\)?\s*$/);
      if (m) return strip(m[1]) + ':' + m[2];
    }
    return '';
  };

  // Explicit annotations (react-dev-inspector, babel/vite source plugins).
  for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
    const p = n.getAttribute('data-inspector-relative-path');
    if (p) return { comp: '', file: p + (n.getAttribute('data-inspector-line') ? ':' + n.getAttribute('data-inspector-line') : '') };
    const ds = n.getAttribute('data-source-file') || n.getAttribute('data-locatorjs-id');
    if (ds) return { comp: n.getAttribute('data-component') || '', file: ds };
  }

  // React: the element's own fiber knows where its JSX was written; its owner
  // (or nearest function ancestor) is the component.
  for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
    const k = Object.keys(n).find((k) => k.startsWith('__reactFiber$') || k.startsWith('__reactInternalInstance$'));
    if (!k) continue;
    let fiber = n[k], file = '', comp = '';
    for (let f = fiber; f && !file; f = f.return) {
      if (f._debugSource) file = strip(f._debugSource.fileName) + ':' + f._debugSource.lineNumber;
      else if (f._debugStack) file = fromStack(f._debugStack);
    }
    const owner = fiber._debugOwner;
    if (owner && typeof owner.type !== 'string') comp = nameOf(owner.type);
    for (let f = fiber; f && !comp; f = f.return) {
      if (f.type && typeof f.type !== 'string') comp = nameOf(f.type);
    }
    if (comp || file) return { comp, file };
    break;
  }

  // Vue 3.
  for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
    const c = n.__vueParentComponent;
    if (c && c.type) return { comp: c.type.name || c.type.__name || '', file: strip(c.type.__file || '') };
  }

  // Svelte (dev builds).
  for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
    const m = n.__svelte_meta;
    if (m && m.loc) return { comp: '', file: strip(m.loc.file) + ':' + (m.loc.line + 1) };
  }
  return null;
}
