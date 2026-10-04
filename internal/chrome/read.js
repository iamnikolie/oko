// Called with `this` = scope element (or window). Returns {title, url,
// source, markdown}: the page's main content as markdown, without site
// chrome, controls or hidden text. Deterministic; no model involved.
function (opts) {
  const doc = document;
  const ws = (s) => (s || '').replace(/[ \t\r\n ]+/g, ' ');
  const SKIP = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'SVG', 'CANVAS', 'BUTTON',
    'OPTION', 'HEAD', 'LINK', 'META', 'OBJECT', 'EMBED', 'AUDIO', 'VIDEO', 'MAP', 'DIALOG']);
  const CHROME = 'nav, aside, [role=navigation], [role=banner], [role=contentinfo], [role=complementary], [role=search]';

  function shortURL(h) {
    try {
      const u = new URL(h, doc.baseURI);
      if (u.protocol === 'javascript:') return '';
      return u.origin === location.origin ? (u.pathname + u.search + u.hash) : u.href;
    } catch (e) { return h; }
  }

  const TRACKING = /^(utm_\w+|trk\w*|refId|trackingId|position|pageNum|fbclid|gclid|yclid|mc_cid|mc_eid|ref_src|_hsenc|_hsmi|igshid|si)$/i;
  function stripTracking(url) {
    try {
      const abs = new URL(url, location.href);
      let changed = false;
      for (const k of [...abs.searchParams.keys()]) {
        if (TRACKING.test(k)) { abs.searchParams.delete(k); changed = true; }
      }
      if (!changed) return url;
      const q = abs.searchParams.toString();
      const rel = !/^https?:/.test(url);
      return (rel ? abs.pathname : abs.origin + abs.pathname) + (q ? '?' + q : '') + abs.hash;
    } catch (e) { return url; }
  }

  function hidden(el, st) {
    if (el.hidden || el.getAttribute('aria-hidden') === 'true' || el.inert) return true;
    if (st.display === 'none' || st.visibility === 'hidden' || st.visibility === 'collapse') return true;
    const cls = typeof el.className === 'string' ? el.className : '';
    if (/\b(sr-only|visually-hidden|screen-reader-text)\b/.test(cls)) return true;
    if (st.position === 'absolute' && st.clip === 'rect(0px, 0px, 0px, 0px)') return true;
    return false;
  }

  function isChrome(el, st) {
    if (opts.full) return false;
    if (el.matches(CHROME)) return true;
    // Page-level header/footer (not ones inside an article or card).
    if ((el.tagName === 'HEADER' || el.tagName === 'FOOTER') && !el.closest('article, main, [role=main], section')) return true;
    // Floating layers: cookie banners, chat bubbles, toasts, devtools.
    if (st.position === 'fixed') return true;
    const name = (typeof el.className === 'string' ? el.className : '') + ' ' + (el.id || '');
    if (BOILER.test(name)) return true;
    return false;
  }
  const BOILER = /(^|[\s_-])(related|recommend(ed|ations)?|share|sharing|social|newsletter|subscribe|promo|advert|ads|sponsor(ed)?|breadcrumbs?|cookie|consent|editsection)([\s_-]|$)/i;

  // A grid of short <article> cards after real prose is "more posts", not
  // the content. On an index page (little prose) the cards are the content.
  function isCardTail(el) {
    if (opts.full || el === root || prose < 1500) return false;
    const arts = el.querySelectorAll(':scope > article, :scope > * > article');
    if (arts.length < 3) return false;
    for (const a of arts) if (textLen(a) > 400) return false;
    return true;
  }

  function textLen(el) { return ws(el.innerText || '').trim().length; }

  // ---- choose the content root ------------------------------------------
  function pickRoot() {
    if (this_el) return [this_el, 'element'];
    if (opts.full) return [doc.body, 'page'];
    // display:contents elements have no box of their own; judge by text.
    const vis = (e) => { const r = e.getBoundingClientRect(); return (r.width > 0 && r.height > 0) || textLen(e) > 0; };
    const mains = [...doc.querySelectorAll('main, [role=main]')].filter(vis);
    if (mains.length) {
      mains.sort((a, b) => textLen(b) - textLen(a));
      return [mains[0], 'main'];
    }
    const bodyLen = textLen(doc.body) || 1;
    const arts = [...doc.querySelectorAll('article')].filter(vis);
    if (arts.length === 1 && textLen(arts[0]) > 0.3 * bodyLen) return [arts[0], 'article'];
    // Readability-lite: credit paragraph text to parent and grandparent.
    const score = new Map();
    let total = 0;
    for (const p of doc.querySelectorAll('p, pre, blockquote')) {
      const t = ws(p.innerText).trim();
      if (t.length < 40) continue;
      let linkLen = 0;
      for (const a of p.querySelectorAll('a')) linkLen += ws(a.innerText).trim().length;
      const s = Math.max(0, t.length - linkLen);
      total += s;
      const par = p.parentElement, gp = par && par.parentElement;
      if (par) score.set(par, (score.get(par) || 0) + s);
      if (gp) score.set(gp, (score.get(gp) || 0) + s / 2);
    }
    let best = null, bestS = 0;
    for (const [el, s] of score) if (s > bestS && el !== doc.body) { best = el; bestS = s; }
    if (best && bestS >= 400 && bestS >= 0.5 * total) {
      // Climb while the parent adds little besides the best block.
      while (best.parentElement && best.parentElement !== doc.body && textLen(best.parentElement) < textLen(best) * 1.25) best = best.parentElement;
      return [best, 'content block'];
    }
    return [doc.body, 'page'];
  }

  // ---- tables --------------------------------------------------------------
  const ROW = 'tr, [role=row]';
  const CELL = 'td, th, [role=cell], [role=gridcell], [role=columnheader], [role=rowheader]';
  function ownRows(t) {
    return [...t.querySelectorAll(ROW)].filter((r) => r.closest('table, [role=table], [role=grid], [role=treegrid]') === t);
  }
  function ownCells(r) {
    const direct = [...r.children].filter((c) => c.matches(CELL));
    if (direct.length) return direct;
    return [...r.querySelectorAll(CELL)].filter((c) => c.closest(ROW) === r);
  }
  function isDataTable(t) {
    if (t.matches('[role=table], [role=grid], [role=treegrid]')) return true;
    if (t.querySelector('table')) return false;
    return !!t.querySelector('th, thead');
  }
  function cellText(c) {
    return ws(render(c, { inTable: true })).trim().replace(/\|/g, '\\|');
  }
  function renderTable(t) {
    const rows = ownRows(t).filter((r) => { const st = getComputedStyle(r); return !hidden(r, st); });
    if (!rows.length) return '';
    if (!isDataTable(t)) {
      // Layout table: one line per row, cells joined.
      const out = [];
      for (const r of rows) {
        const cells = ownCells(r).map((c) => squash(render(c, {}))).filter(Boolean);
        if (!cells.length) continue;
        out.push(cells.some((c) => c.includes('\n')) ? cells.join('\n') : cells.join(' '));
      }
      return '\n\n' + out.join('\n') + '\n\n';
    }
    let header = null;
    let body = rows;
    const first = ownCells(rows[0]);
    if (first.length && first.every((c) => c.matches('th, [role=columnheader]'))) {
      header = first.map(cellText);
      body = rows.slice(1);
    }
    // Expand rowspan/colspan so every row has its full set of columns.
    const carry = [];
    const data = [];
    for (const r of body) {
      const out = [];
      let col = 0;
      const fill = () => {
        while (carry[col] && carry[col].left > 0) {
          out.push(carry[col].text);
          carry[col].left--;
          col++;
        }
      };
      for (const c of ownCells(r)) {
        fill();
        const t = cellText(c);
        const cs = Math.max(1, Math.min(20, +(c.getAttribute('colspan') || 1)));
        const rs = Math.max(1, Math.min(500, +(c.getAttribute('rowspan') || 1)));
        for (let k = 0; k < cs; k++) {
          out.push(t);
          if (rs > 1) carry[col] = { text: t, left: rs - 1 };
          col++;
        }
      }
      fill();
      if (out.some((c) => c)) data.push(out);
    }
    const width = Math.max(header ? header.length : 0, ...data.map((r) => r.length));
    if (!width) return '';
    const pad = (r) => { const o = r.slice(0, width); while (o.length < width) o.push(''); return o; };
    const line = (r) => '| ' + pad(r).join(' | ') + ' |';
    const out = [];
    out.push(line(header || Array.from({ length: width }, () => '')));
    out.push('|' + ' --- |'.repeat(width));
    const max = opts.rows > 0 ? opts.rows : data.length;
    for (const r of data.slice(0, max)) out.push(line(r));
    if (data.length > max) out.push(`\n… ${data.length - max} more rows (--rows 0 for all)`);
    return '\n\n' + out.join('\n') + '\n\n';
  }

  // ---- lists ---------------------------------------------------------------
  function renderList(el, ordered) {
    let items = [...el.children].filter((c) => c.tagName === 'LI' || c.getAttribute('role') === 'listitem');
    // role=list with plain divs inside (common in app markup): every child
    // element is an item, or nothing would be rendered at all.
    if (!items.length) items = [...el.children];
    let n = parseInt(el.getAttribute('start') || '1', 10) || 1;
    const out = [];
    for (const li of items) {
      const st = getComputedStyle(li);
      if (hidden(li, st)) continue;
      let body = squash(render(li, {}));
      if (!body) continue;
      body = compactItem(body);
      const mark = ordered ? (n++) + '. ' : '- ';
      out.push(mark + body.split('\n').join('\n' + ' '.repeat(mark.length)));
    }
    return out.length ? '\n\n' + out.join('\n') + '\n\n' : '';
  }

  // A short list item (a search result, a feed card) reads best as one line:
  // its overlay URL folded into the title, headings demoted, parts joined.
  function compactItem(body) {
    let lines = body.split('\n').filter((l) => l.trim() && l.trim() !== '---');
    // Fold a card's overlay URL ("→ url", wherever it landed) into its title.
    const at = lines.findIndex((l) => l.startsWith('→ '));
    if (at >= 0 && lines.length > 1) {
      const url = lines[at].slice(2);
      lines.splice(at, 1);
      const title = lines[0].replace(/^#{1,6} /, '').replace(/^\*\*(.*)\*\*$/, '$1');
      // The first line may itself be a compacted card ("Name · Role · City");
      // link only its first part.
      const cut = title.indexOf(' · ');
      const head = cut > 0 ? title.slice(0, cut) : title, rest = cut > 0 ? title.slice(cut) : '';
      if (!/\]\(/.test(head)) lines[0] = '[' + head + '](' + url + ')' + rest;
      else lines.push('→ ' + url);
    }
    if (lines.length > 10 || lines.some((l) => l.length > 240 || /^(- |\d+\. |\||```|> )/.test(l))) return lines.join('\n');
    return lines.map((l) => l.replace(/^#{1,6} /, '')).join(' · ');
  }

  // ---- generic rendering ---------------------------------------------------
  function children(el, ctx) {
    let s = '';
    const kids = el.shadowRoot ? [...el.shadowRoot.childNodes, ...el.childNodes] : el.childNodes;
    for (const n of kids) s += render(n, ctx);
    if (el.tagName === 'IFRAME') {
      try { if (el.contentDocument && el.contentDocument.body) s += render(el.contentDocument.body, ctx); } catch (e) { /* cross-origin */ }
    }
    return s;
  }

  function render(node, ctx) {
    if (node.nodeType === 3) return ws(node.textContent);
    if (node.nodeType !== 1) return '';
    const el = node;
    if (SKIP.has(el.tagName) && !(el.tagName === 'DIALOG' && el.open)) return '';
    const st = getComputedStyle(el);
    if (hidden(el, st)) return '';
    const s = renderEl(el, st, ctx);
    // An inline element set apart by CSS margin or padding ("Hacker News" then
    // "new" with no space in the markup) reads as separate words.
    if (!s || !/^inline/.test(st.display)) return s;
    const gap = (a, b) => parseFloat(a) > 0 || parseFloat(b) > 0;
    return (gap(st.marginLeft, st.paddingLeft) ? ' ' : '') + s + (gap(st.marginRight, st.paddingRight) ? ' ' : '');
  }

  function renderEl(el, st, ctx) {
    const tag = el.tagName;
    if (el !== root && isChrome(el, st)) return '';
    if (isCardTail(el)) { omitted++; return ''; }

    const block = !/^inline/.test(st.display) && st.display !== 'contents';
    const B = (s) => (s.trim() ? '\n\n' + s.trim() + '\n\n' : '');
    switch (tag) {
      case 'H1': case 'H2': case 'H3': case 'H4': case 'H5': case 'H6': {
        const t = ws(children(el, ctx)).trim();
        return t ? '\n\n' + '#'.repeat(+tag[1]) + ' ' + t + '\n\n' : '';
      }
      case 'BR': return '\n';
      case 'SUP': {
        // Citation markers ([3], [a], [note 1]) are noise without the notes.
        const t = ws(el.textContent).trim();
        if (/^\[[^\]]{1,12}\]$/.test(t)) return '';
        break;
      }
      case 'HR': return '\n\n---\n\n';
      case 'PRE': {
        const code = el.querySelector('code');
        const lang = ((code && code.className) || el.className || '').match(/(?:language|lang)-([\w+-]+)/);
        const t = (el.innerText || el.textContent || '').replace(/\n+$/, '');
        return '\n\n```' + (lang ? lang[1] : '') + '\n' + t + '\n```\n\n';
      }
      case 'CODE': {
        const t = el.textContent;
        return t.trim() ? '`' + t.replace(/`/g, 'ˋ') + '`' : '';
      }
      case 'STRONG': case 'B': {
        const t = children(el, ctx);
        return t.trim() && !ctx.inTable ? '**' + t.trim() + '**' : t;
      }
      case 'EM': case 'I': {
        const t = children(el, ctx);
        return t.trim() && !ctx.inTable ? '_' + t.trim() + '_' : t;
      }
      case 'A': {
        const inner = children(el, ctx);
        const t = squash(inner);
        const h = el.getAttribute('href');
        let url = h && h !== '#' ? shortURL(h) : '';
        if (url && opts.links !== 'all') url = stripTracking(url);
        if (!t) {
          // Card-wide overlay link whose text is screen-reader only: the
          // card's URL is the information, so keep it as its own line.
          if (url && opts.links !== 'none' && ws(el.textContent).trim()) return '\n→ ' + url + '\n';
          return '';
        }
        let keep = !!url && opts.links !== 'none';
        if (keep && opts.links !== 'all') {
          // Smart: keep where the URL carries information — links that leave
          // the site, and links whose text reads like a title.
          const external = /^https?:/.test(url) && !url.startsWith(location.origin);
          keep = external || t.length >= 25;
        }
        if (!keep) return block ? B(inner) : inner;
        if (opts.links !== 'all' && url.length > 120) {
          // Tracking-heavy URLs: the path is the information, the query is noise.
          const q = url.indexOf('?');
          if (q > 0) url = url.slice(0, q) + '?…';
        }
        const hm = t.match(/^(#{1,6}) ([^\n]+)$/);
        if (hm) return '\n\n' + hm[1] + ' [' + hm[2] + '](' + url + ')\n\n';
        if (t.includes('\n')) return '\n\n' + t + '\n→ ' + url + '\n\n';
        return '[' + t + '](' + url + ')';
      }
      case 'IMG': {
        const alt = ws(el.getAttribute('alt')).trim();
        if (opts.images && alt) return '![' + alt + '](' + shortURL(el.currentSrc || el.src) + ')';
        return '';
      }
      case 'INPUT': {
        const ty = (el.type || 'text').toLowerCase();
        if (ty === 'hidden' || ty === 'password' || ty === 'submit' || ty === 'button' || ty === 'reset' || ty === 'file') return '';
        if (ty === 'checkbox' || ty === 'radio') {
          // Visually hidden toggles drive menus; only real controls count.
          const r = el.getBoundingClientRect();
          if (st.opacity === '0' || r.width <= 2 || r.height <= 2) return '';
          return el.checked ? '[x] ' : '[ ] ';
        }
        return el.value ? ' ' + ws(el.value).trim() + ' ' : '';
      }
      case 'TEXTAREA': return el.value ? B(el.value) : '';
      case 'SELECT': {
        const o = el.selectedOptions && el.selectedOptions[0];
        return o ? ' ' + ws(o.text).trim() + ' ' : '';
      }
      case 'UL': case 'MENU': return renderList(el, false);
      case 'OL': return renderList(el, true);
      case 'TABLE': return renderTable(el);
      case 'BLOCKQUOTE': {
        const t = squash(children(el, ctx));
        return t ? '\n\n' + t.split('\n').map((l) => '> ' + l).join('\n') + '\n\n' : '';
      }
      case 'DT': { const t = ws(children(el, ctx)).trim(); return t ? '\n**' + t + '**\n' : ''; }
      case 'DD': { const t = squash(children(el, ctx)); return t ? ': ' + t + '\n' : ''; }
    }
    const role = el.getAttribute('role');
    if (role === 'table' || role === 'grid' || role === 'treegrid') return renderTable(el);
    if (role === 'list') return renderList(el, false);
    if (role === 'heading') {
      const t = ws(children(el, ctx)).trim();
      const lvl = +(el.getAttribute('aria-level') || 2);
      return t ? '\n\n' + '#'.repeat(Math.min(6, Math.max(1, lvl))) + ' ' + t + '\n\n' : '';
    }
    const inner = children(el, ctx);
    if (ctx.inTable) return inner + ' ';
    if (block) {
      const c = compact(el, inner);
      return c !== null ? '\n\n' + c + '\n\n' : B(inner);
    }
    // Inline-block/flex chips sit side by side; keep them apart.
    return /inline-(block|flex|grid)/.test(st.display) ? ' ' + inner + ' ' : inner;
  }

  // App UIs are div soup: a card or stat tile renders as a stack of tiny
  // lines ("Orders pending", "1"). A short block with no prose structure
  // reads better as one line: "Orders pending · 1".
  function compact(el, inner) {
    const t = squash(inner);
    if (!t.includes('\n') || t.length > 200) return null;
    if (/^(#|\||- |\d+\. |> |```)/m.test(t)) return null;
    if (el.querySelector('li, pre, table, blockquote, h1, h2, h3, h4, h5, h6, br')) return null;
    const lines = t.split('\n').filter(Boolean);
    if (lines.some((l) => l.length > 100)) return null;
    // Several <p> are prose unless every line is label-short.
    if (el.querySelectorAll('p').length > 1 && lines.some((l) => l.length > 40)) return null;
    // Lines that are already compacted cards keep their own separator.
    const sep = lines.some((l) => l.includes(' · ')) ? ' | ' : ' · ';
    // Prose split across elements ("A,", "B,", "and", "C") joins with spaces.
    const conn = /^(and|or|&|и|или|та|і)$/i;
    let out = lines[0];
    for (let i = 1; i < lines.length; i++) {
      const prev = lines[i - 1], cur = lines[i];
      out += (/[,;:]$/.test(prev) || conn.test(prev) || conn.test(cur)) ? ' ' : sep;
      out += cur;
    }
    return out;
  }

  // Tidy a fragment: trim lines, collapse blank runs and repeated spaces.
  function squash(s) {
    const lines = s.split('\n').map((l) => l.replace(/[ \t]+/g, ' ').trim());
    const out = [];
    for (const l of lines) {
      if (!l && (!out.length || out[out.length - 1] === '')) continue;
      out.push(l);
    }
    while (out.length && out[out.length - 1] === '') out.pop();
    return out.join('\n');
  }

  const this_el = (this && this.nodeType === 1) ? this : null;
  const [root, source] = pickRoot();
  let prose = 0;
  for (const p of root.querySelectorAll('p')) prose += ws(p.innerText).trim().length;
  let omitted = 0;
  let md = squash(render(root, {}));
  // Glue separators that layout put on their own line ("A", ",", "B", "and", "C").
  md = md.replace(/\n+(,|;|·|\||&|and|or|и|или)\n+/g, (m, sep) => (sep === ',' || sep === ';' ? sep + ' ' : ' ' + sep + ' '));
  md = md.replace(/,\n+(?=[^\n#>|`-])/g, ', ');
  if (omitted) md += '\n\n_(' + omitted + ' block(s) of related cards omitted; --full keeps them)_';
  // Drop immediately repeated lines (responsive duplicates, "Read more" twins).
  const lines = md.split('\n');
  md = lines.filter((l, i) => !(l && i > 0 && l === lines[i - 1])).join('\n');
  return { title: doc.title, url: location.href, source, markdown: md };
}
