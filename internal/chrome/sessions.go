package chrome

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Session is one caller's view of a profile: its current tab, where that tab
// was when the caller last looked, and the tabs it opened. Each session lives
// in its own file, so callers sharing a profile never overwrite each other.
type Session struct {
	Key string `json:"key"`
	// Tab is the target id of this session's current tab.
	Tab string `json:"tab,omitempty"`
	// URL is where Tab was when this session's last command finished.
	URL string `json:"url,omitempty"`
	// Opened lists the tabs this session owns: opened or claimed by it.
	Opened []string `json:"opened,omitempty"`

	prof *Profile
}

// SessionTTL is how long a session keeps its tabs without running a command.
// After that its file is dropped and its tabs become free to take.
const SessionTTL = 6 * time.Hour

var keyBadRe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// sessionFile maps a caller key to a safe file name.
func sessionFile(key string) string {
	k := keyBadRe.ReplaceAllString(key, "_")
	if len(k) > 80 {
		k = k[:80]
	}
	if k == "" || k[0] == '.' {
		k = "_" + k
	}
	return k + ".json"
}

func (p *Profile) sessionsDir() string { return filepath.Join(p.Dir, "sessions") }

// Session loads (or starts) the session for a caller key.
func (p *Profile) Session(key string) *Session {
	s := &Session{Key: key, prof: p}
	if b, err := os.ReadFile(filepath.Join(p.sessionsDir(), sessionFile(key))); err == nil {
		_ = json.Unmarshal(b, s)
	}
	return s
}

func (s *Session) Save() error {
	dir := s.prof.sessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	path := filepath.Join(dir, sessionFile(s.Key))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Owns reports whether this session opened or claimed the tab.
func (s *Session) Owns(id string) bool {
	for _, o := range s.Opened {
		if strings.EqualFold(o, id) {
			return true
		}
	}
	return false
}

// Own records the tab as this session's.
func (s *Session) Own(id string) {
	if !s.Owns(id) {
		s.Opened = append(s.Opened, id)
	}
}

// Prune forgets tabs that no longer exist.
func (s *Session) Prune(live map[string]bool) {
	var keep []string
	for _, o := range s.Opened {
		if live[o] {
			keep = append(keep, o)
		}
	}
	s.Opened = keep
	if s.Tab != "" && !live[s.Tab] {
		s.Tab, s.URL = "", ""
	}
}

// Owners maps tab id to the key of the session owning it, across every live
// session of the profile. Sessions idle longer than SessionTTL are removed.
func (p *Profile) Owners() map[string]string {
	out := map[string]string{}
	ents, err := os.ReadDir(p.sessionsDir())
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(p.sessionsDir(), e.Name())
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > SessionTTL {
			_ = os.Remove(path)
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var v struct {
			Key    string   `json:"key"`
			Opened []string `json:"opened"`
		}
		if json.Unmarshal(b, &v) != nil {
			continue
		}
		key := v.Key
		if key == "" {
			key = strings.TrimSuffix(e.Name(), ".json")
		}
		for _, id := range v.Opened {
			out[id] = key
		}
	}
	return out
}
