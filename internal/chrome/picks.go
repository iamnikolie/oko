package chrome

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

//go:embed picker.js
var PickerJS string

//go:embed source.js
var SourceJS string

// PickWorld is the isolated world the picker runs in; PickBinding is the
// function it reports through, exposed in that world only.
const (
	PickWorld   = "oko"
	PickBinding = "__okoPick"
)

// MainSourceJS installs, once per document, the main-world listener the
// picker asks for an element's component and source location.
var MainSourceJS = `(() => {
  if (window.__okoSrcArmed) return;
  window.__okoSrcArmed = true;
  ` + SourceJS + `
  document.addEventListener('oko-src', (e) => {
    const el = e.composedPath()[0];
    let r = null;
    try { r = okoSource(el); } catch (err) { /* ignore */ }
    document.documentElement.setAttribute('data-oko-src', r ? (r.comp || '') + '|' + (r.file || '') : '|');
  }, true);
})()`

// PickResolveJS finds the element the picker tagged, registers it as a
// snapshot ref (same numbering as 'oko snap'), and returns the ref, its
// source location and where to crop it.
var PickResolveJS = `function (id, seqBase) {
  ` + SourceJS + `
  const sel = '[data-oko-pick="' + id + '"]';
  const deep = (root) => {
    const hit = root.querySelector(sel);
    if (hit) return hit;
    for (const n of root.querySelectorAll('*')) if (n.shadowRoot) { const f = deep(n.shadowRoot); if (f) return f; }
    return null;
  };
  const el = deep(document);
  if (!el) return null;
  el.removeAttribute('data-oko-pick');
  if (!window.__okoRefs) {
    window.__okoRefs = new Map();
    window.__okoIds = new WeakMap();
    window.__okoSeq = seqBase || 0;
  }
  let ref = window.__okoIds.get(el);
  if (!ref) { ref = 'e' + (++window.__okoSeq); window.__okoIds.set(el, ref); }
  window.__okoRefs.set(ref, el);
  let src = null;
  try { src = okoSource(el); } catch (e) { /* ignore */ }
  const r = el.getBoundingClientRect();
  return { ref, seq: window.__okoSeq, comp: (src && src.comp) || '', file: (src && src.file) || '',
    x: r.left + scrollX, y: r.top + scrollY, w: r.width, h: r.height };
}`

// Pick is one element the user pointed at in the browser.
type Pick struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Tab   string    `json:"tab"`
	URL   string    `json:"url"`
	Title string    `json:"title,omitempty"`
	N     int       `json:"n"`
	Ref   string    `json:"ref,omitempty"`
	Role  string    `json:"role"`
	Name  string    `json:"name"`
	Ctx   string    `json:"ctx,omitempty"`
	Sel   string    `json:"sel,omitempty"`
	W     int       `json:"w"`
	H     int       `json:"h"`
	X     int       `json:"x"`
	Y     int       `json:"y"`
	Pad   string    `json:"pad,omitempty"`
	BG    string    `json:"bg,omitempty"`
	Color string    `json:"color,omitempty"`
	Font  string    `json:"font,omitempty"`
	Comp  string    `json:"comp,omitempty"`
	File  string    `json:"file,omitempty"`
	Note  string    `json:"note,omitempty"`
	Shot  string    `json:"shot,omitempty"`
}

// PickMode is the picker's last mode change, for 'oko pick' to know when the
// user is done.
type PickMode struct {
	Tab    string    `json:"tab"`
	On     bool      `json:"on"`
	Picked int       `json:"picked"`
	At     time.Time `json:"at"`
}

// ClearedBy marks picks dropped with 'oko picks --clear' (no session read them).
const ClearedBy = "!clear"

const keepPicks = 200

func (p *Profile) PicksDir() string     { return filepath.Join(p.Dir, "picks") }
func (p *Profile) picksLog() string     { return filepath.Join(p.PicksDir(), "picks.jsonl") }
func (p *Profile) ConsumedPath() string { return filepath.Join(p.PicksDir(), "consumed.json") }
func (p *Profile) pickModePath() string { return filepath.Join(p.PicksDir(), "mode.json") }

// lockPicks serializes writers of the picks files across processes.
func (p *Profile) lockPicks() (func(), error) {
	if err := os.MkdirAll(p.PicksDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.PicksDir(), "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// AddPick appends a pick, keeping the log to the last keepPicks entries.
func (p *Profile) AddPick(pk Pick) error {
	unlock, err := p.lockPicks()
	if err != nil {
		return err
	}
	defer unlock()
	all := p.readPicks()
	all = append(all, pk)
	if len(all) > keepPicks {
		for _, old := range all[:len(all)-keepPicks] {
			if old.Shot != "" {
				_ = os.Remove(old.Shot)
			}
		}
		all = all[len(all)-keepPicks:]
		var b strings.Builder
		for _, x := range all {
			line, _ := json.Marshal(x)
			b.Write(line)
			b.WriteByte('\n')
		}
		return writeAtomic(p.picksLog(), []byte(b.String()))
	}
	f, err := os.OpenFile(p.picksLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(pk)
	_, err = f.Write(append(line, '\n'))
	return err
}

// Picks returns every pick in the log, oldest first.
func (p *Profile) Picks() []Pick { return p.readPicks() }

func (p *Profile) readPicks() []Pick {
	f, err := os.Open(p.picksLog())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Pick
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var pk Pick
		if json.Unmarshal(sc.Bytes(), &pk) == nil && pk.ID != "" {
			out = append(out, pk)
		}
	}
	return out
}

// Consumed maps pick id to the session that read it (or ClearedBy).
func (p *Profile) Consumed() map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(p.ConsumedPath()); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// Consume marks picks as read by a session. Picks already read stay with
// their first reader.
func (p *Profile) Consume(ids []string, by string) error {
	if len(ids) == 0 {
		return nil
	}
	unlock, err := p.lockPicks()
	if err != nil {
		return err
	}
	defer unlock()
	c := p.Consumed()
	for _, id := range ids {
		if _, ok := c[id]; !ok || by == ClearedBy {
			c[id] = by
		}
	}
	// Forget ids that fell out of the log.
	live := map[string]bool{}
	for _, pk := range p.readPicks() {
		live[pk.ID] = true
	}
	for id := range c {
		if !live[id] {
			delete(c, id)
		}
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeAtomic(p.ConsumedPath(), b)
}

func (p *Profile) SetPickMode(m PickMode) error {
	if err := os.MkdirAll(p.PicksDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	return writeAtomic(p.pickModePath(), b)
}

func (p *Profile) PickMode() (PickMode, bool) {
	var m PickMode
	b, err := os.ReadFile(p.pickModePath())
	if err != nil || json.Unmarshal(b, &m) != nil {
		return m, false
	}
	return m, true
}

// Profiles lists the names of every profile under Home.
func Profiles() []string {
	ents, err := os.ReadDir(filepath.Join(Home(), "profiles"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && nameRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
