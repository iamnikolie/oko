package chrome

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process ownership. oko signals only processes it started itself. When it
// starts one (Chrome, the idle reaper, the picker watcher, the proxy relay)
// it writes a record: the pid, the start time and the executable as the OS
// reports them (sysctl kern.proc + proc_pidpath on macOS, /proc on Linux),
// the canonical oko home and the record's own canonical location (profile
// and purpose). Before every signal the record is checked
// against the OS again: same pid, same start time, same executable, same
// home, read from the same place it was written to. A pid that was reused, a process that merely looks like Chrome or
// oko, a record copied from another home: none of them match, so they are
// never signalled; a record that no longer matches is removed. Nothing is
// inferred from command lines.

// procRecord is what oko wrote down about a process it started.
type procRecord struct {
	PID   int    `json:"pid"`
	Start string `json:"start"` // OS start time, exact (see osIdentity)
	Exe   string `json:"exe"`   // executable path as the OS resolves it
	Home  string `json:"home"`  // canonical oko home it was started for
	// Path is where the record was written, canonical: it names the home,
	// the profile and the purpose (chrome, idle, watch, relay, a tab's
	// recorder or holder). A record read anywhere else (copied to another
	// profile, another tab, another home) proves nothing.
	Path string `json:"path"`
}

// errNoIdentity: the OS cannot describe the process (gone, or unsupported).
var errNoIdentity = errors.New("process identity unavailable")

// canonicalHome is Home with symlinks resolved, so two spellings of one home
// match and two homes never do.
func canonicalHome() string {
	h := Home()
	if a, err := filepath.Abs(h); err == nil {
		h = a
	}
	if r, err := filepath.EvalSymlinks(h); err == nil {
		h = r
	}
	return h
}

// canonicalPath resolves the directory of path (the file itself may not
// exist yet), so one location has one spelling.
func canonicalPath(path string) string {
	if a, err := filepath.Abs(path); err == nil {
		path = a
	}
	dir, base := filepath.Split(path)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return filepath.Join(dir, base)
}

// resolveExe resolves a path the way the OS reports executables.
func resolveExe(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// SelfExe is this oko binary, resolved; its helpers run the same file.
func SelfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return resolveExe(exe)
}

// writeAtomic writes b to path through a temp file of its own and a rename,
// so a reader sees the old content or the new one, never a mix, whatever
// other writers do at the same time.
func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// RecordProc writes down the process pid that oko just started: it must run
// wantExe and must have started no earlier than since (zero: no bound).
func RecordProc(path string, pid int, wantExe string, since time.Time) error {
	start, key, exe, err := osIdentity(pid)
	if err != nil {
		return err
	}
	if wantExe == "" || exe != resolveExe(wantExe) {
		return errors.New("process runs " + exe + ", not " + wantExe)
	}
	// Start times are exact on macOS and 10 ms ticks against a boot time in
	// whole seconds on Linux: allow for that rounding.
	if !since.IsZero() && start.Before(since.Add(-2*time.Second)) {
		return errors.New("process started before oko launched it")
	}
	b, _ := json.Marshal(procRecord{PID: pid, Start: key, Exe: exe, Home: canonicalHome(), Path: canonicalPath(path)})
	return writeAtomic(path, b)
}

// OwnedPID returns the pid recorded at path while the OS still describes the
// recorded process, else 0. A record that does not match is removed: it
// can only ever be stale or foreign.
func OwnedPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var r procRecord
	if json.Unmarshal(b, &r) != nil || r.PID <= 0 {
		_ = os.Remove(path)
		return 0
	}
	if r.Home != canonicalHome() {
		// Another home's record (a copied profile dir): not ours to judge
		// or to clean up.
		return 0
	}
	if r.Path == "" || r.Path != canonicalPath(path) {
		// Written for another place (another profile, tab or purpose), or
		// by an oko that did not bind records to a place: proves nothing
		// here, and is no record of anything that lives here.
		_ = os.Remove(path)
		return 0
	}
	_, key, exe, err := osIdentity(r.PID)
	if err != nil || key != r.Start || exe != r.Exe {
		_ = os.Remove(path)
		return 0
	}
	return r.PID
}

// SignalOwned sends sig to the process recorded at path, checking the record
// against the OS at the moment of signalling. It reports whether it sent.
func SignalOwned(path string, sig syscall.Signal) bool {
	pid := OwnedPID(path)
	if pid == 0 {
		return false
	}
	return syscall.Kill(pid, sig) == nil
}

// lockHolder reads the pid from Chrome's SingletonLock ("<host>-<pid>"); 0
// when the lock is missing, malformed or held on another host. It is an
// observation only (is a browser on this dir?), never a signal target.
func (p *Profile) lockHolder() int {
	target, err := os.Readlink(filepath.Join(p.UserDataDir(), "SingletonLock"))
	if err != nil {
		return 0
	}
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return 0
	}
	if host, err := os.Hostname(); err != nil || target[:i] != host {
		return 0
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// LockHolder is the live pid named by the profile's Chrome lock, or 0. Use it
// to see that some browser holds the dir; to signal, use PID.
func (p *Profile) LockHolder() int {
	pid := p.lockHolder()
	if pid == 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

func (p *Profile) chromeRecordPath() string { return filepath.Join(p.Dir, "chrome.proc") }

// PID is the pid of the Chrome oko launched for this profile, proven against
// the record written at launch; 0 when not running or not provably oko's
// (e.g. a browser an older oko started: CDP can still close that one).
func (p *Profile) PID() int { return OwnedPID(p.chromeRecordPath()) }

// recordLaunch writes down the Chrome that just started on this profile:
// the process holding its lock, running bin, started after since.
func (p *Profile) recordLaunch(bin string, since time.Time) {
	_ = os.Remove(p.chromeRecordPath())
	if pid := p.lockHolder(); pid != 0 {
		_ = RecordProc(p.chromeRecordPath(), pid, bin, since)
	}
}

// SignalChrome signals the Chrome oko launched for this profile, proven
// against its launch record at this moment; false when there is none.
func (p *Profile) SignalChrome(sig syscall.Signal) bool {
	return SignalOwned(p.chromeRecordPath(), sig)
}
