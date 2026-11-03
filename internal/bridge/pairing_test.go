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
	if _, err := p.Pair("ABCD-EFGH", testOrigin); err != errNoCode {
		t.Fatalf("pair without a code: %v", err)
	}
	code, err := p.NewCode()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(code) || strings.ContainsAny(code, "01OIL") {
		t.Fatalf("code %q", code)
	}
	if _, err := p.Pair(code, "https://evil.example"); err != errBadOrigin {
		t.Fatalf("web origin paired: %v", err)
	}
	if _, err := p.Pair("WRON-GCOD", testOrigin); err != errCodeWrong {
		t.Fatalf("wrong code: %v", err)
	}
	// Case and separators do not matter.
	token, err := p.Pair(strings.ToLower(strings.ReplaceAll(code, "-", " ")), testOrigin)
	if err != nil || len(token) != 64 {
		t.Fatalf("pair: %q %v", token, err)
	}
	if _, err := p.Pair(code, testOrigin); err != errNoCode {
		t.Fatalf("code worked twice: %v", err)
	}
	if !p.Verify(token, testOrigin) {
		t.Fatal("token not accepted")
	}
	if p.Verify(token, "chrome-extension://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Fatal("token accepted from another extension")
	}
	if p.Verify(token[:63]+"0", testOrigin) || p.Verify("", testOrigin) {
		t.Fatal("wrong token accepted")
	}

	// The pairing survives a restart; only the hash is on disk.
	q := LoadPairing(p.path)
	if !q.Verify(token, testOrigin) || q.Paired() != testOrigin {
		t.Fatal("pairing not persisted")
	}
	if b := readFile(t, p.path); strings.Contains(b, token) {
		t.Fatal("token stored in clear")
	}
	if err := q.Unpair(); err != nil || q.Verify(token, testOrigin) || q.Paired() != "" {
		t.Fatalf("unpair: %v", err)
	}
}

func TestPairingCodeBurnsAndExpires(t *testing.T) {
	p := LoadPairing(filepath.Join(t.TempDir(), "paired.json"))
	code, _ := p.NewCode()
	for i := 0; i < codeAttempts-1; i++ {
		if _, err := p.Pair("XXXX-XXXX", testOrigin); err != errCodeWrong {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := p.Pair("XXXX-XXXX", testOrigin); err != errCodeBurned {
		t.Fatalf("burn: %v", err)
	}
	if _, err := p.Pair(code, testOrigin); err != errNoCode {
		t.Fatalf("burned code still works: %v", err)
	}

	now := time.Now()
	p.now = func() time.Time { return now }
	code, _ = p.NewCode()
	p.now = func() time.Time { return now.Add(codeTTL + time.Second) }
	if _, err := p.Pair(code, testOrigin); err != errCodeExpired {
		t.Fatalf("expired code: %v", err)
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
