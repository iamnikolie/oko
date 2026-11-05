package chrome

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Leases. Every command that drives the browser, and a running recorder,
// holds a shared flock on the profile's use.lock for as long as it works; the
// idle reaper closes the browser only while it holds the lock exclusively.
// So the reaper never closes a browser under a live command, and a command
// that starts while the reaper is closing waits and then starts a fresh
// browser. The kernel drops a lock with its process, so a crashed command
// leaves no stale lease. Files from os.OpenFile are close-on-exec: helpers a
// command starts (reaper, watcher) do not inherit its lease.

func (p *Profile) leasePath() string { return filepath.Join(p.Dir, "use.lock") }

// Use takes a shared lease, waiting (bounded by ctx) while the reaper closes
// the browser. Call the returned func to release it.
func (p *Profile) Use(ctx context.Context) (func(), error) {
	f, err := os.OpenFile(p.leasePath(), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
			return func() { _ = f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("profile %s: the idle browser is still closing: %w", p.Name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Exclusive takes the lease alone, without waiting; false while any command
// or recorder holds it.
func (p *Profile) Exclusive() (func(), bool) {
	f, err := os.OpenFile(p.leasePath(), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, false
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()
		return nil, false
	}
	return func() { _ = f.Close() }, true
}

// The idle limit in force for the running browser lives in idle.limit, one
// file every process reads: each command writes the limit it resolves
// (--idle stored for the profile, else its $OKO_IDLE, else DefaultIdle), the
// reaper and 'oko status' read it. So the last command's setting is the one
// the reaper applies and the one status shows, whatever environment the
// reaper itself was started with.

func (p *Profile) idleLimitPath() string { return filepath.Join(p.Dir, "idle.limit") }

// ApplyIdle makes this caller's limit the one in force.
func (p *Profile) ApplyIdle() {
	v := "off"
	if d := p.IdleTTL(); d > 0 {
		v = d.String()
	}
	if b, err := os.ReadFile(p.idleLimitPath()); err == nil && string(b) == v {
		return
	}
	tmp := p.idleLimitPath() + ".tmp"
	if os.WriteFile(tmp, []byte(v), 0o600) == nil {
		_ = os.Rename(tmp, p.idleLimitPath())
	}
}

// IdleLimit is the limit in force: the applied one while it exists, else
// what this caller would apply. 0 means never close.
func (p *Profile) IdleLimit() time.Duration {
	if b, err := os.ReadFile(p.idleLimitPath()); err == nil {
		if d, ok := ParseIdle(string(b)); ok {
			return d
		}
	}
	return p.IdleTTL()
}

// ClearIdle drops the applied limit once the browser is gone.
func (p *Profile) ClearIdle() { _ = os.Remove(p.idleLimitPath()) }
