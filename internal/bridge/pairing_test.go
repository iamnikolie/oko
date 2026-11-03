package bridge

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testOrigin = "chrome-extension://abcdefghijklmnopabcdefghijklmnop"

func TestPairingCode(t *testing.T) {
	p := LoadPairing(filepath.Join(t.TempDir(), "paired.json"))
	if _, err := p.beginPair(testOrigin); err != errNoCode {
		t.Fatalf("pair without a code: %v", err)
	}
	code, err := p.NewCode()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(code) || strings.ContainsAny(code, "01OIL") {
		t.Fatalf("code %q", code)
	}
	if _, err := p.beginPair("https://evil.example"); err != errBadOrigin {
		t.Fatalf("web origin paired: %v", err)
	}
	got, err := p.beginPair(testOrigin)
	if err != nil || got != normCode(code) {
		t.Fatalf("begin: %q %v", got, err)
	}
	key := make([]byte, 32)
	key[0] = 7
	if err := p.completePair(got, key, testOrigin); err != nil {
		t.Fatal(err)
	}
	if _, err := p.beginPair(testOrigin); err != errNoCode {
		t.Fatalf("code worked twice: %v", err)
	}
	if k, ok := p.key(testOrigin); !ok || k[0] != 7 {
		t.Fatal("key not stored")
	}
	if _, ok := p.key("chrome-extension://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); ok {
		t.Fatal("key handed to another extension")
	}

	q := LoadPairing(p.path)
	if _, ok := q.key(testOrigin); !ok || q.Paired() != testOrigin {
		t.Fatal("pairing not persisted")
	}
	if err := q.Unpair(); err != nil || q.Paired() != "" {
		t.Fatalf("unpair: %v", err)
	}
	if _, ok := q.key(testOrigin); ok {
		t.Fatal("key still there after unpair")
	}
}

func TestPairingCodeBurnsAndExpires(t *testing.T) {
	p := LoadPairing(filepath.Join(t.TempDir(), "paired.json"))
	_, _ = p.NewCode()
	for i := 0; i < codeAttempts; i++ {
		if _, err := p.beginPair(testOrigin); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := p.beginPair(testOrigin); err != errCodeBurned {
		t.Fatalf("burn: %v", err)
	}
	if _, err := p.beginPair(testOrigin); err != errNoCode {
		t.Fatalf("burned code still works: %v", err)
	}

	now := time.Now()
	p.now = func() time.Time { return now }
	_, _ = p.NewCode()
	p.now = func() time.Time { return now.Add(codeTTL + time.Second) }
	if _, err := p.beginPair(testOrigin); err != errCodeExpired {
		t.Fatalf("expired code: %v", err)
	}
}

func TestOldPairingIsUnpaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paired.json")
	if err := writeFile(path, `{"token_sha256":"ab","origin":"`+testOrigin+`"}`); err != nil {
		t.Fatal(err)
	}
	if LoadPairing(path).Paired() != "" {
		t.Fatal("a token-hash pairing from before mutual authentication still counts")
	}
}

func TestRestrictedURL(t *testing.T) {
	for _, u := range []string{"chrome://settings", "chrome-extension://x/popup.html", "devtools://devtools/x",
		"https://chromewebstore.google.com/detail/x", "https://chrome.google.com/webstore/detail/x", "about:version",
		"view-source:https://a.b", "edge://flags"} {
		if restrictedURL(u) == "" {
			t.Errorf("%s allowed", u)
		}
	}
	for _, u := range []string{"", "about:blank", "https://example.com", "http://localhost:3000/x", "https://chrome.google.com/search", "data:text/html,x"} {
		if why := restrictedURL(u); why != "" {
			t.Errorf("%s refused: %s", u, why)
		}
	}
}

func TestPolicy(t *testing.T) {
	for _, m := range []string{"Runtime.evaluate", "Input.dispatchMouseEvent", "Page.captureScreenshot", "DOM.setFileInputFiles"} {
		if why := clientAllowed(m, nil); why != "" {
			t.Errorf("%s refused: %s", m, why)
		}
	}
	for _, m := range []string{"Network.getAllCookies", "Network.getCookies", "Network.setCookie", "Storage.getCookies",
		"Storage.clearDataForOrigin", "Target.createTarget", "Target.getTargets", "Target.attachToTarget", "Target.setAutoAttach",
		"Target.closeTarget", "Browser.getVersion", "SystemInfo.getInfo", "Tracing.start", "IO.read", "Fetch.enable",
		"Page.setDownloadBehavior", "Browser.setDownloadBehavior"} {
		if clientAllowed(m, nil) == "" {
			t.Errorf("%s allowed", m)
		}
	}
	nav := func(u string) string { return clientAllowed("Page.navigate", []byte(`{"url":"`+u+`"}`)) }
	for _, u := range []string{"https://a.example/", "http://localhost:1/x", "about:blank"} {
		if why := nav(u); why != "" {
			t.Errorf("navigate %s refused: %s", u, why)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "chrome://settings", "javascript:alert(1)", "data:text/html,x", "about:version"} {
		if nav(u) == "" {
			t.Errorf("navigate %s allowed", u)
		}
	}
}
