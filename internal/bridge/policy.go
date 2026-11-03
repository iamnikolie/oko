package bridge

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"

	"github.com/iamnikolie/oko/extension"
)

// cdpPolicy is the allow-list of per-tab CDP methods, shared with the
// extension (extension/cdp-policy.json); each side enforces it on its own.
type cdpPolicy struct {
	Client []string `json:"client"`
	Bridge []string `json:"bridge"`
}

var (
	policyOnce   sync.Once
	clientMethod map[string]bool
	bridgeMethod map[string]bool
)

func loadPolicy() {
	policyOnce.Do(func() {
		var p cdpPolicy
		b, err := extension.Files.ReadFile("cdp-policy.json")
		if err == nil {
			_ = json.Unmarshal(b, &p)
		}
		clientMethod, bridgeMethod = map[string]bool{}, map[string]bool{}
		for _, m := range p.Client {
			clientMethod[m] = true
		}
		for _, m := range p.Bridge {
			bridgeMethod[m] = true
		}
	})
}

// clientAllowed explains why an oko command may not send method to a
// handed-over tab, or returns "".
func clientAllowed(method string, params json.RawMessage) string {
	loadPolicy()
	if !clientMethod[method] {
		return method + ": not allowed through the oko extension (only what oko's page commands need; " +
			"cookies, storage, other tabs and the browser stay out of reach)"
	}
	if method == "Page.navigate" {
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(params, &p)
		return navigableURL(p.URL)
	}
	return ""
}

// navigableURL explains why a handed-over tab may not be sent to u, or "".
func navigableURL(u string) string {
	if why := restrictedURL(u); why != "" {
		return why
	}
	if u == "about:blank" {
		return ""
	}
	pu, err := url.Parse(u)
	if err != nil {
		return "bad URL"
	}
	switch strings.ToLower(pu.Scheme) {
	case "http", "https":
		return ""
	}
	return pu.Scheme + ": URLs cannot be opened through the extension (http, https and about:blank only)"
}
