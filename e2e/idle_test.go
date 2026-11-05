package e2e

import (
	"os"
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
	must(t, oko(t, append(p, "status")...), "closes after 30m idle")
	must(t, oko(t, "!", "-p", "idleenv", "up", "--idle", "soon"), "want a duration")
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
