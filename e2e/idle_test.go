package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNoWebdriverFlag(t *testing.T) {
	oko(t, "open", base+"/form.html")
	must(t, oko(t, "eval", "String(navigator.webdriver)"), "false")
}

// An idle browser closes by itself and the next command starts it again.
func TestIdleClose(t *testing.T) {
	p := []string{"-p", "idle"}
	defer oko(t, append(p, "down")...)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "2s")...), "closes after 2s idle")
	// Commands keep it alive past the limit.
	for i := 0; i < 3; i++ {
		time.Sleep(time.Second)
		oko(t, append(p, "eval", "1")...)
	}
	must(t, oko(t, append(p, "status")...), "running")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(oko(t, append(p, "status")...), "stopped") {
		if time.Now().After(deadline) {
			t.Fatal("idle browser still running")
		}
		time.Sleep(300 * time.Millisecond)
	}
	must(t, oko(t, append(p, "eval", "1")...), "1")
	oko(t, append(p, "up", "--idle", "off")...)
	must(t, oko(t, append(p, "status")...), "running", "never closes when idle")
}

// waitStopped polls a profile's status until its browser is gone.
func waitStopped(t *testing.T, p []string, within time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(oko(t, append(p, "status")...), "stopped") {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// $OKO_IDLE sets the limit for profiles without their own; 'up --idle'
// wins over it and '--idle default' goes back to it.
func TestIdleEnv(t *testing.T) {
	p := []string{"-p", "idleenv"}
	defer oko(t, append(p, "down")...)
	t.Setenv("OKO_IDLE", "2s")
	must(t, oko(t, append(p, "up", "--headless")...), "closes after 2s idle")
	waitStopped(t, p, 10*time.Second, "browser still running past $OKO_IDLE")
	must(t, oko(t, append(p, "up", "--headless", "--idle", "off")...), "never closes when idle")
	must(t, oko(t, append(p, "up", "--idle", "default")...), "closes after 2s idle")
	t.Setenv("OKO_IDLE", "")
	must(t, oko(t, append(p, "status")...), "closes after 2s idle") // the limit in force
	oko(t, append(p, "eval", "1")...)
	must(t, oko(t, append(p, "status")...), "closes after 30m idle")
	must(t, oko(t, "!", "-p", "idleenv", "up", "--idle", "soon"), "want a duration")
}

// A reaper started under one $OKO_IDLE follows the limit a later command
// applies: status and the reaper never disagree.
func TestIdleEnvLiveReaper(t *testing.T) {
	p := []string{"-p", "idleenv2"}
	defer oko(t, append(p, "down")...)
	t.Setenv("OKO_IDLE", "2s")
	must(t, oko(t, append(p, "up", "--headless")...), "closes after 2s idle")
	t.Setenv("OKO_IDLE", "off")
	must(t, oko(t, append(p, "up")...), "never closes when idle")
	must(t, oko(t, append(p, "eval", "new Promise(r => setTimeout(() => r(42), 3000))", "--timeout", "10s")...), "42")
	time.Sleep(3 * time.Second)
	must(t, oko(t, append(p, "status")...), "running", "never closes when idle")
}

// A command that runs longer than the limit is not closed under itself.
func TestIdleLongCommand(t *testing.T) {
	p := []string{"-p", "idlelong"}
	defer oko(t, append(p, "down")...)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "2s")...), "closes after 2s idle")
	oko(t, append(p, "open", base+"/form.html")...)
	out := oko(t, append([]string{"!"}, append(p, "wait", "--text", "never on this page", "--timeout", "5s")...)...)
	mustNot(t, out, "closed", "not running", "disconnected")
	must(t, oko(t, append(p, "status")...), "running")
	waitStopped(t, p, 10*time.Second, "idle browser still running after the long command")
}

// A browser whose reaper died is closed once any oko command runs, on any
// profile: nothing else would ever close it.
func TestIdleOrphan(t *testing.T) {
	p := []string{"-p", "idleorphan"}
	defer oko(t, append(p, "down")...)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "2s")...), "closes after 2s idle")
	pidFile := filepath.Join(home, "profiles", "idleorphan", "idle.pid")
	var reaper int
	for i := 0; i < 30 && reaper == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		reaper, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(100 * time.Millisecond)
	}
	if reaper == 0 {
		t.Fatal("no reaper pid")
	}
	if err := syscall.Kill(reaper, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	must(t, oko(t, append(p, "status")...), "running")
	// A command on another profile notices the orphan.
	oko(t, "eval", "1")
	waitStopped(t, p, 10*time.Second, "orphaned browser still running")
}

// okoBg starts oko in the background; wait() returns its output and error.
func okoBg(t *testing.T, args ...string) (wait func() (string, error)) {
	t.Helper()
	c := exec.Command(bin, args...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	t.Cleanup(func() { _ = c.Process.Kill() })
	return func() (string, error) {
		select {
		case err := <-done:
			return out.String(), err
		case <-time.After(60 * time.Second):
			t.Fatalf("oko %v did not finish", args)
			return "", nil
		}
	}
}

// leaseHeld reports whether some command holds the profile's lease.
func leaseHeld(t *testing.T, profile string) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(home, "profiles", profile, "use.lock"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

const slowEval = "new Promise(r => setTimeout(() => r(42), 3000))"

// Turning the limit on while a command runs does not close the browser
// under it.
func TestIdleOnUnderCommand(t *testing.T) {
	p := []string{"-p", "idleon"}
	defer oko(t, append(p, "down")...)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "off")...), "never closes when idle")
	wait := okoBg(t, append(p, "eval", slowEval, "--timeout", "20s")...)
	time.Sleep(700 * time.Millisecond)
	must(t, oko(t, append(p, "up", "--idle", "2s")...), "closes after 2s idle")
	out, err := wait()
	if err != nil || !strings.Contains(out, "42") {
		t.Fatalf("command closed under itself: %v\n%s", err, out)
	}
	waitStopped(t, p, 10*time.Second, "idle browser still running after the command")
}

// The reaper has decided the browser is idle; a command starts before it
// acts. The reaper must give way.
func TestIdleCommandBeforeClose(t *testing.T) {
	gate := t.TempDir()
	p := []string{"-p", "idlerace1"}
	defer oko(t, append(p, "down")...)
	t.Setenv("OKO_TEST_REAP_GATE", gate)
	_ = os.WriteFile(filepath.Join(gate, "locked.go"), nil, 0o600)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "2s")...), "closes after 2s idle")
	waitFile(t, filepath.Join(gate, "decide.reached"))
	wait := okoBg(t, append(p, "eval", slowEval, "--timeout", "20s")...)
	for i := 0; !leaseHeld(t, "idlerace1"); i++ {
		if i > 200 {
			t.Fatal("command never took the lease")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.WriteFile(filepath.Join(gate, "decide.go"), nil, 0o600)
	out, err := wait()
	if err != nil || !strings.Contains(out, "42") {
		t.Fatalf("reaper closed the browser under a live command: %v\n%s", err, out)
	}
	mustNot(t, out, "started chrome")
	waitStopped(t, p, 10*time.Second, "idle browser still running after the command")
}

// The reaper holds the lease and is closing; a command arriving now waits,
// then gets a fresh browser instead of failing.
func TestIdleCommandDuringClose(t *testing.T) {
	gate := t.TempDir()
	p := []string{"-p", "idlerace2"}
	defer oko(t, append(p, "down")...)
	t.Setenv("OKO_TEST_REAP_GATE", gate)
	_ = os.WriteFile(filepath.Join(gate, "decide.go"), nil, 0o600)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "2s")...), "closes after 2s idle")
	waitFile(t, filepath.Join(gate, "locked.reached"))
	t.Setenv("OKO_TEST_REAP_GATE", "")
	wait := okoBg(t, append(p, "eval", "6*7", "--timeout", "30s")...)
	time.Sleep(time.Second)
	must(t, oko(t, append(p, "status")...), "running") // the command is waiting
	_ = os.WriteFile(filepath.Join(gate, "locked.go"), nil, 0o600)
	out, err := wait()
	if err != nil || !strings.Contains(out, "42") {
		t.Fatalf("command arriving during close failed: %v\n%s", err, out)
	}
	must(t, out, "started chrome")
}

// A stale lock whose pid now belongs to an unrelated process: no command,
// sweep, reaper or 'oko down' may signal that process.
func TestIdleStaleLockSignalsNothing(t *testing.T) {
	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- victim.Wait() }()
	defer victim.Process.Kill()

	dir := filepath.Join(home, "profiles", "idlestale")
	if err := os.MkdirAll(filepath.Join(dir, "chrome"), 0o700); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if err := os.Symlink(host+"-"+strconv.Itoa(victim.Process.Pid), filepath.Join(dir, "chrome", "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"headless":true,"idle":"1s"}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "last_used"), nil, 0o600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "last_used"), old, old)

	oko(t, "eval", "1") // sweeps other profiles
	must(t, oko(t, "-p", "idlestale", "status"), "stopped")
	oko(t, "-p", "idlestale", "down")
	select {
	case err := <-exited:
		t.Fatalf("unrelated process was signalled: %v", err)
	case <-time.After(3 * time.Second):
	}
	if _, err := os.Stat(filepath.Join(dir, "idle.pid")); err == nil {
		t.Fatal("a reaper was started for a process that is not the profile's Chrome")
	}
}
