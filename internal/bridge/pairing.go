package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	// codeAttempts wrong guesses burn the code.
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
	errCodeBurned  = errors.New("too many wrong codes; run 'oko ext pair' again")
	errBadOrigin   = errors.New("pairing is only accepted from a Chrome extension")
)

// Pairing holds the one-time code and the paired extension: the hash of its
// token and its origin. Only the hash is stored; the token lives in the
// extension's storage.
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
	TokenSHA256 string    `json:"token_sha256"`
	Origin      string    `json:"origin"`
	PairedAt    time.Time `json:"paired_at"`
}

// LoadPairing reads the pairing state from path (missing means unpaired).
func LoadPairing(path string) *Pairing {
	p := &Pairing{path: path, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &p.saved)
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

// Pair checks the code from an extension at origin and, when it matches,
// stores a new token for that extension (replacing any earlier pairing) and
// returns it. The code works once.
func (p *Pairing) Pair(code, origin string) (string, error) {
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
	if subtle.ConstantTimeCompare([]byte(normCode(code)), []byte(p.code)) != 1 {
		p.attempts++
		if p.attempts >= codeAttempts {
			p.code = ""
			return "", errCodeBurned
		}
		return "", errCodeWrong
	}
	p.code = ""
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	saved := pairedFile{TokenSHA256: hex.EncodeToString(sum[:]), Origin: origin, PairedAt: p.now().UTC()}
	b, _ := json.MarshalIndent(saved, "", "  ")
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return "", err
	}
	p.saved = saved
	return token, nil
}

// Verify reports whether token is the paired extension's and origin is its
// origin.
func (p *Pairing) Verify(token, origin string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved.TokenSHA256 == "" || token == "" || origin != p.saved.Origin {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(p.saved.TokenSHA256)) == 1
}

// Paired returns the paired extension's origin, or "".
func (p *Pairing) Paired() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved.TokenSHA256 == "" {
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

// Unpair forgets the paired extension; its token stops working.
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
