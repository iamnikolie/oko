package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

// The reaper is a detached `oko _idle` process per running browser. Agents
// rarely run 'oko down', so without it every profile an agent ever touched
// keeps a Chrome (and its watcher and relay) alive. It closes the browser
// once no oko command has driven it for the profile's idle limit, then exits;
// the next command starts the browser again with the same logins.

func reapPidPath(p *chrome.Profile) string { return filepath.Join(p.Dir, "idle.pid") }

// reapPID is the pid of the profile's live reaper, proven by its command line.
func reapPID(p *chrome.Profile) int {
	b, err := os.ReadFile(reapPidPath(p))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return chrome.Helper(pid, "_idle", p.Name)
}

func reapLockPath(p *chrome.Profile) string { return filepath.Join(p.Dir, "idle.lock") }

// reaperRunning reports whether a reaper holds the profile's reaper lock.
// The kernel drops the lock with the reaper, so a dead reaper or a reused pid
// never looks alive, and no process needs to be inspected.
func reaperRunning(p *chrome.Profile) bool {
	f, err := os.OpenFile(reapLockPath(p), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
}

// used marks the browser as driven, makes this caller's idle limit the one
// in force, makes sure a reaper watches it, and gives a reaper to any other
// profile's browser that lost its own.
func used(p *chrome.Profile) {
	p.Touch()
	if !p.State.Extension {
		p.ApplyIdle()
	}
	ensureReaper(p)
	sweepOrphans(p.Name)
}

// ensureReaper starts the profile's reaper unless one is running. The user's
// own Chrome (extension profile) is never closed for being idle.
func ensureReaper(p *chrome.Profile) {
	if p.State.Extension || p.IdleLimit() == 0 || reaperRunning(p) {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(self, "_idle", "--profile", p.Name)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if c.Start() == nil {
		go func() { _ = c.Wait() }()
	}
}

// sweepOrphans gives a reaper to every other profile whose own Chrome is
// running without one: its reaper was killed or crashed, or an oko without
// reapers started it. Nothing else would ever close such a browser, since
// only a command on that same profile starts its reaper. The reaper closes
// it at once when it has been idle past the limit. A profile's Chrome counts
// only when proven to be that profile's (chrome.Profile.PID): a stale lock
// whose pid now belongs to another process is left alone.
func sweepOrphans(skip string) {
	ents, err := os.ReadDir(filepath.Join(chrome.Home(), "profiles"))
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || e.Name() == skip {
			continue
		}
		dir := filepath.Join(chrome.Home(), "profiles", e.Name())
		if _, err := os.Lstat(filepath.Join(dir, "chrome", "SingletonLock")); err != nil {
			continue
		}
		p, err := chrome.Load(e.Name())
		// Cheap checks first; PID (which inspects the process) only for a
		// live lock that no reaper watches.
		if err != nil || p.State.Extension || !p.LockAlive() || reaperRunning(p) || p.PID() == 0 {
			continue
		}
		ensureReaper(p)
	}
}

// stopBrowser closes the profile's Chrome and the processes riding along
// with it. It reports false when Chrome was not running. Only processes
// proven to belong to the profile are signalled.
func stopBrowser(ctx context.Context, p *chrome.Profile) (bool, error) {
	if p.State.Extension {
		return stopExtension(ctx)
	}
	pid := p.PID()
	if ws, err := p.Endpoint(ctx); err == nil {
		b := rod.New().ControlURL(ws).Context(ctx)
		if b.Connect() == nil {
			_ = proto.BrowserClose{}.Call(b)
		} else if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	} else if pid == 0 {
		p.StopRelay()
		p.ClearIdle()
		return false, nil
	} else {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for i := 0; i < 50 && p.PID() != 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if pid := p.PID(); pid != 0 {
		return true, fmt.Errorf("chrome (pid %d) did not exit", pid)
	}
	p.StopRelay()
	p.ClearIdle()
	if pid := watchPID(p); pid != 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if pid := reapPID(p); pid != 0 && pid != os.Getpid() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return true, nil
}

// reapGate is a test hook: with $OKO_TEST_REAP_GATE set to a directory, the
// reaper stops at the named point, says so with <dir>/<name>.reached, and
// goes on once <dir>/<name>.go exists. It lets tests order the reaper and a
// command exactly.
func reapGate(name string) {
	dir := os.Getenv("OKO_TEST_REAP_GATE")
	if dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, name+".reached"), nil, 0o600)
	for i := 0; i < 1200; i++ {
		if _, err := os.Stat(filepath.Join(dir, name+".go")); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var idleCmd = &cobra.Command{
	Use:    "_idle",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p0, err := chrome.Load(profileName)
		if err != nil {
			return err
		}
		// One reaper per profile: the one holding the lock. Retry briefly,
		// since reaperRunning probes it for an instant.
		lock, err := os.OpenFile(reapLockPath(p0), os.O_CREATE|os.O_RDONLY, 0o600)
		if err != nil {
			return err
		}
		defer lock.Close()
		for i := 0; syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil; i++ {
			if i >= 20 {
				return nil // another reaper watches this profile
			}
			time.Sleep(50 * time.Millisecond)
		}
		_ = os.WriteFile(reapPidPath(p0), []byte(strconv.Itoa(os.Getpid())), 0o600)
		defer os.Remove(reapPidPath(p0))
		for {
			// Reload each round: a command may have changed the limit.
			p, err := chrome.Load(profileName)
			if err != nil {
				return err
			}
			ttl := p.IdleLimit()
			if ttl == 0 || p.PID() == 0 {
				return nil
			}
			idle := time.Since(p.LastUsed())
			if idle < ttl {
				time.Sleep(reapTick(ttl - idle))
				continue
			}
			reapGate("decide")
			closed, err := reapOnce(p)
			if closed {
				return err
			}
			time.Sleep(reapTick(ttl))
		}
	},
}

// reapOnce closes the browser if it is still idle once the reaper holds the
// lease alone; false when a command or a recording holds it, when it was
// used meanwhile, or when the user has the window in front.
func reapOnce(p *chrome.Profile) (bool, error) {
	release, ok := p.Exclusive()
	if !ok {
		return false, nil
	}
	defer release()
	// Decide again under the lease: a command may have just finished or
	// changed the limit.
	ttl := p.IdleLimit()
	if ttl == 0 || p.PID() == 0 {
		return true, nil
	}
	if time.Since(p.LastUsed()) < ttl {
		return false, nil
	}
	reapGate("locked")
	if p.Frontmost() {
		// Someone is using the window by hand.
		p.Touch()
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pid := p.PID()
	_, err := stopBrowser(ctx, p)
	if err != nil && pid != 0 && p.PID() == pid {
		// A hung Chrome ignored BrowserClose and SIGTERM; nothing else
		// would ever close it. PID proved again that it is still ours.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		for i := 0; i < 30 && p.PID() != 0; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if p.PID() == 0 {
			p.StopRelay()
			p.ClearIdle()
			if w := watchPID(p); w != 0 {
				_ = syscall.Kill(w, syscall.SIGTERM)
			}
			err = nil
		}
	}
	return true, err
}

// shortDur prints 1h, 30m, 1h30m instead of 1h0m0s.
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// reapTick sleeps until the limit could be reached, checking at least every
// minute so a changed limit or a closed browser is noticed.
func reapTick(left time.Duration) time.Duration {
	if left < time.Second {
		return time.Second
	}
	if left > time.Minute {
		return time.Minute
	}
	return left
}

func init() {
	rootCmd.AddCommand(idleCmd)
}
