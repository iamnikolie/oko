package bridge

import (
	"net/url"
	"strings"
)

// restrictedURL explains why the extension cannot drive a page at u, or
// returns "". Chrome refuses extension debuggers on its own pages, the Web
// Store and other extensions' pages; oko refuses them first with a clear
// message instead of a detached tab.
func restrictedURL(u string) string {
	if u == "" {
		return ""
	}
	pu, err := url.Parse(u)
	if err != nil {
		return ""
	}
	switch strings.ToLower(pu.Scheme) {
	case "chrome", "chrome-untrusted", "edge", "brave", "opera", "vivaldi":
		return "browser pages (" + pu.Scheme + "://) cannot be driven through the extension"
	case "chrome-extension", "moz-extension":
		return "extension pages cannot be driven through the extension"
	case "devtools":
		return "DevTools pages cannot be driven through the extension"
	case "view-source", "chrome-search":
		return pu.Scheme + ": pages cannot be driven through the extension"
	case "about":
		if pu.Opaque != "blank" && pu.Opaque != "srcdoc" {
			return "about: pages other than about:blank cannot be driven through the extension"
		}
	}
	host := strings.ToLower(pu.Hostname())
	if host == "chromewebstore.google.com" || host == "chrome.google.com" && strings.HasPrefix(pu.Path, "/webstore") {
		return "the Chrome Web Store cannot be driven through the extension"
	}
	return ""
}
