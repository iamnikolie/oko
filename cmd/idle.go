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

// used marks the browser as driven and makes sure a reaper watches it.
func used(p *chrome.Profile) {
	p.Touch()
	if p.IdleTTL() == 0 || reapPID(p) != 0 {
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

// stopBrowser closes the profile's Chrome and the processes riding along
// with it. It reports false when Chrome was not running.
func stopBrowser(ctx context.Context, p *chrome.Profile) (bool, error) {
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
				if p.Frontmost() {
					// Someone is using the window by hand.
					p.Touch()
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					_, err := stopBrowser(ctx, p)
					cancel()
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
