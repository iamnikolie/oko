package chrome

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Process identity. A pid read from a file (Chrome's SingletonLock, a pid
// file) may by now belong to an unrelated process, so oko signals a process
// only after proving it is the one it means: its command line has to carry
// this profile's arguments, and a Chrome oko launched has to have the start
// time oko recorded. When ownership cannot be proven, nothing is signalled.

type procInfo struct {
	start string // lstart, C locale: "Thu Nov  5 11:40:25 2026"
	args  string // full command line, arguments joined by spaces
}

// lookProc describes a live process, or reports false.
func lookProc(pid int) (procInfo, bool) {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return procInfo{}, false
	}
	c := exec.Command("ps", "-ww", "-o", "lstart=,command=", "-p", strconv.Itoa(pid))
	c.Env = append(os.Environ(), "LC_ALL=C")
	out, err := c.Output()
	if err != nil {
		return procInfo{}, false
	}
	s := strings.TrimLeft(strings.TrimRight(string(out), "\n"), " ")
	// lstart is fixed width (24) in the C locale.
	if len(s) < 26 || strings.Count(s, "\n") > 0 {
		return procInfo{}, false
	}
	return procInfo{start: s[:24], args: strings.TrimSpace(s[24:])}, true
}

// hasArgs reports whether the command line holds seq as whole arguments.
func hasArgs(args, seq string) bool {
	return strings.Contains(" "+args+" ", " "+seq+" ")
}

// Helper returns pid when it is a live `oko <verb> --profile <profile>`
// process (the idle reaper, the picker watcher, the proxy relay), else 0.
func Helper(pid int, verb, profile string) int {
	pr, ok := lookProc(pid)
	if !ok || !hasArgs(pr.args, verb+" --profile "+profile) {
		return 0
	}
	return pid
}

// lockPID reads the pid from Chrome's SingletonLock ("<host>-<pid>"); 0 when
// the lock is missing, malformed or held on another host.
func (p *Profile) lockPID() int {
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

// LockAlive is a cheap first check, no proof of identity: Chrome's lock
// names a live pid on this host. PID proves it.
func (p *Profile) LockAlive() bool {
	pid := p.lockPID()
	return pid != 0 && syscall.Kill(pid, 0) == nil
}

func (p *Profile) launchIDPath() string { return filepath.Join(p.Dir, "chrome.id") }

// isOurChrome reports whether pid is this profile's Chrome: started with its
// user-data dir and oko's debugging flag, and, when oko recorded the launch
// of this pid, started at the recorded time.
func (p *Profile) isOurChrome(pid int) (procInfo, bool) {
	pr, ok := lookProc(pid)
	if !ok || !hasArgs(pr.args, "--user-data-dir="+p.UserDataDir()) || !hasArgs(pr.args, "--remote-debugging-port=0") {
		return procInfo{}, false
	}
	if b, err := os.ReadFile(p.launchIDPath()); err == nil {
		if id := strings.SplitN(string(b), "\n", 2); len(id) == 2 && id[0] == strconv.Itoa(pid) && id[1] != pr.start {
			return procInfo{}, false
		}
	}
	return pr, true
}

// recordLaunch remembers which process the Chrome just launched is.
func (p *Profile) recordLaunch() {
	_ = os.Remove(p.launchIDPath())
	pid := p.lockPID()
	if pr, ok := p.isOurChrome(pid); ok {
		_ = os.WriteFile(p.launchIDPath(), []byte(strconv.Itoa(pid)+"\n"+pr.start), 0o600)
	}
}
