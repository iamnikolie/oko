package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// codeTTL is how long a pairing code printed by 'oko ext pair' works.
	codeTTL = 10 * time.Minute
	// codeAttempts pairing handshakes burn the code, whatever their outcome.
	codeAttempts = 5
	// codeAlphabet has no 0/O, 1/I/L: the code is read off a terminal.
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	codeLen      = 8
)

// extOriginRe is a Chrome extension origin: 32 letters a-p.
var extOriginRe = regexp.MustCompile(`^chrome-extension://[a-p]{32}$`)

var (
	errNoCode      = errors.New("no pairing code is open; run 'oko ext pair' and use the code it prints")
	errCodeExpired = errors.New("pairing code expired; run 'oko ext pair' again")
	errCodeWrong   = errors.New("wrong pairing code")
	errCodeBurned  = errors.New("too many pairing attempts; run 'oko ext pair' again")
	errBadOrigin   = errors.New("pairing is only accepted from a Chrome extension")
)

// Pairing holds the one-time code and the paired extension: its origin and
// the link key both sides derived during pairing. The key sits in a 0600 file
// and in the extension's storage; it never crosses the socket.
type Pairing struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	code     string
	codeExp  time.Time
	attempts int
	saved    pairedFile
}

type pairedFile struct {
	Key      string    `json:"key"`
	Origin   string    `json:"origin"`
	PairedAt time.Time `json:"paired_at"`
}

// LoadPairing reads the pairing state from path (missing means unpaired; a
// pairing from before mutual authentication counts as unpaired).
func LoadPairing(path string) *Pairing {
	p := &Pairing{path: path, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &p.saved)
	}
	if k, err := hex.DecodeString(p.saved.Key); err != nil || len(k) != 32 {
		p.saved = pairedFile{}
	}
	return p
}

// NewCode opens a fresh pairing code, replacing any open one.
func (p *Pairing) NewCode() (string, error) {
	b := make([]byte, codeLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code = string(b)
	p.codeExp = p.now().Add(codeTTL)
	p.attempts = 0
	return p.code[:4] + "-" + p.code[4:], nil
}

// normCode drops separators and case, so "k7qf 3mzd" equals "K7QF-3MZD".
func normCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

// beginPair spends one attempt of the open code for an extension at origin
// and returns the code to key the handshake with.
func (p *Pairing) beginPair(origin string) (string, error) {
	if !extOriginRe.MatchString(origin) {
		return "", errBadOrigin
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == "" {
		return "", errNoCode
	}
	if p.now().After(p.codeExp) {
		p.code = ""
		return "", errCodeExpired
	}
	if p.attempts >= codeAttempts {
		p.code = ""
		return "", errCodeBurned
	}
	p.attempts++
	return p.code, nil
}

// completePair stores the link key for origin; the code is used up.
func (p *Pairing) completePair(code string, key []byte, origin string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code != code {
		return errNoCode
	}
	p.code = ""
	saved := pairedFile{Key: hex.EncodeToString(key), Origin: origin, PairedAt: p.now().UTC()}
	b, _ := json.MarshalIndent(saved, "", "  ")
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return err
	}
	p.saved = saved
	return nil
}

// key returns the link key when origin is the paired extension's.
func (p *Pairing) key(origin string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved.Key == "" || origin != p.saved.Origin {
		return nil, false
	}
	k, err := hex.DecodeString(p.saved.Key)
	return k, err == nil
}

// Paired returns the paired extension's origin, or "".
func (p *Pairing) Paired() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved.Key == "" {
		return ""
	}
	return p.saved.Origin
}

// CodeOpen reports whether a pairing code is waiting to be entered.
func (p *Pairing) CodeOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code != "" && !p.now().After(p.codeExp)
}

// Unpair forgets the paired extension; its key stops working.
func (p *Pairing) Unpair() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = pairedFile{}
	p.code = ""
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
