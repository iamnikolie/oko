// Pages Chrome never lets an extension debugger drive. Checked before a tab is
// handed over, so the user gets a reason instead of a silent failure.
export function restricted(url) {
  if (!url) return '';
  let u;
  try { u = new URL(url); } catch { return ''; }
  const scheme = u.protocol.slice(0, -1).toLowerCase();
  if (['chrome', 'chrome-untrusted', 'edge', 'brave', 'opera', 'vivaldi', 'chrome-search', 'view-source'].includes(scheme)) {
    return 'Chrome’s own pages (' + scheme + '://) cannot be handed to oko.';
  }
  if (scheme === 'devtools') return 'DevTools pages cannot be handed to oko.';
  if (scheme === 'chrome-extension') return 'Extension pages cannot be handed to oko.';
  if (scheme === 'about' && url !== 'about:blank' && url !== 'about:srcdoc') {
    return 'about: pages other than about:blank cannot be handed to oko.';
  }
  const host = u.hostname.toLowerCase();
  if (host === 'chromewebstore.google.com' || (host === 'chrome.google.com' && u.pathname.startsWith('/webstore'))) {
    return 'The Chrome Web Store cannot be handed to oko.';
  }
  return '';
}
