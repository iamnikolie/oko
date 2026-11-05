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

func reapPID(p *chrome.Profile) int {
	b, err := os.ReadFile(reapPidPath(p))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

// used marks the browser as driven and makes sure a reaper watches it, and
// any other profile's browser that lost its reaper.
func used(p *chrome.Profile) {
	p.Touch()
	ensureReaper(p)
	sweepOrphans(p.Name)
}

// ensureReaper starts the profile's reaper unless one is running. The user's
// own Chrome (extension profile) is never closed for being idle.
func ensureReaper(p *chrome.Profile) {
	if p.State.Extension || p.IdleTTL() == 0 || reapPID(p) != 0 {
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

// sweepOrphans gives a reaper to every other profile whose Chrome is running
// without one: its reaper was killed or crashed, or the browser was started
// by an oko that had none. Nothing else would ever close such a browser,
// since only a command on that same profile starts its reaper. The reaper
// closes it at once when it has been idle past the limit. Cheap enough for
// every command: one readlink per profile, and a state read only for a live
// Chrome without a reaper.
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
		if err != nil || p.PID() == 0 {
			continue
		}
		ensureReaper(p)
	}
}

// keepUsed marks the browser as used while a long command (console
// --follow, picks --wait, net --live, a slow page) runs, so the reaper never
// closes it under a command, and once more when the command ends. Call the
// returned func when done.
func keepUsed(p *chrome.Profile) func() {
	ttl := p.IdleTTL()
	if p.State.Extension || ttl == 0 {
		return func() {}
	}
	every := ttl / 3
	if every > time.Minute {
		every = time.Minute
	}
	if every < 500*time.Millisecond {
		every = 500 * time.Millisecond
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.Touch()
			}
		}
	}()
	return func() {
		close(done)
		p.Touch()
	}
}

// recording reports whether a screencast recorder is still running on any of
// the profile's tabs: an unfinished recording keeps the browser open.
func recording(p *chrome.Profile) bool {
	for _, pid := range p.State.Recorders {
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			return true
		}
	}
	return false
}

// stopBrowser closes the profile's Chrome and the processes riding along
// with it. It reports false when Chrome was not running.
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
	if pid := watchPID(p); pid != 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if pid := reapPID(p); pid != 0 && pid != os.Getpid() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return true, nil
}

var idleCmd = &cobra.Command{
	Use:    "_idle",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if p, err := chrome.Load(profileName); err == nil {
			_ = os.WriteFile(reapPidPath(p), []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		defer func() {
			if p, err := chrome.Load(profileName); err == nil && reapPID(p) == os.Getpid() {
				_ = os.Remove(reapPidPath(p))
			}
		}()
		for {
			// Reload each round: 'oko up --idle' may have changed the limit.
			p, err := chrome.Load(profileName)
			if err != nil {
				return err
			}
			ttl := p.IdleTTL()
			// A second reaper started in a race: the newest pid file wins.
			if ttl == 0 || p.PID() == 0 || reapPID(p) != os.Getpid() {
				return nil
			}
			idle := time.Since(p.LastUsed())
			if idle >= ttl {
				if p.Frontmost() || recording(p) {
					// Someone is using the window by hand, or a recording
					// is still running.
					p.Touch()
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					pid := p.PID()
					_, err := stopBrowser(ctx, p)
					cancel()
					if err != nil && pid != 0 && p.PID() == pid {
						// A hung Chrome ignored BrowserClose and SIGTERM;
						// nothing else would ever close it.
						_ = syscall.Kill(pid, syscall.SIGKILL)
						for i := 0; i < 30 && p.PID() != 0; i++ {
							time.Sleep(100 * time.Millisecond)
						}
						if p.PID() == 0 {
							p.StopRelay()
							if w := watchPID(p); w != 0 {
								_ = syscall.Kill(w, syscall.SIGTERM)
							}
							err = nil
						}
					}
					return err
				}
				idle = 0
			}
			time.Sleep(reapTick(ttl - idle))
		}
	},
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
