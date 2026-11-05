package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	pidFile := filepath.Join(home, "profiles", "idleorphan", "idle.proc")
	var reaper int
	for i := 0; i < 30 && reaper == 0; i++ {
		reaper = procPID(pidFile)
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

// procPID reads the pid from one of oko's process records.
func procPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var r struct {
		PID int `json:"pid"`
	}
	_ = json.Unmarshal(b, &r)
	return r.PID
}

// startVictim starts a process the test owns that no oko may signal.
func startVictim(t *testing.T, name string, args ...string) (int, <-chan error) {
	t.Helper()
	c := exec.Command(name, args...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ch := make(chan error, 1)
	go func() { ch <- c.Wait() }()
	t.Cleanup(func() { _ = c.Process.Kill() })
	time.Sleep(100 * time.Millisecond)
	return c.Process.Pid, ch
}

func mustLive(t *testing.T, exited <-chan error, what string) {
	t.Helper()
	select {
	case err := <-exited:
		t.Fatalf("%s was signalled: %v", what, err)
	case <-time.After(3 * time.Second):
	}
}

// A stale lock whose pid now belongs to a process oko did not launch, plain
// or with an oko Chrome's exact arguments and deaf to SIGTERM: no sweep,
// reaper or 'oko down' may signal it.
func TestIdleStaleLockSignalsNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		args func(dir string) []string
	}{
		{"unrelated", func(string) []string { return []string{"sleep", "60"} }},
		{"matching-args", func(dir string) []string {
			return []string{"sh", "-c", "trap '' TERM; while :; do sleep 1; done", "chrome",
				"--user-data-dir=" + filepath.Join(dir, "chrome"), "--remote-debugging-port=0"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			name := "idlestale-" + c.name
			dir := filepath.Join(home, "profiles", name)
			if err := os.MkdirAll(filepath.Join(dir, "chrome"), 0o700); err != nil {
				t.Fatal(err)
			}
			a := c.args(dir)
			pid, exited := startVictim(t, a[0], a[1:]...)
			host, _ := os.Hostname()
			if err := os.Symlink(host+"-"+strconv.Itoa(pid), filepath.Join(dir, "chrome", "SingletonLock")); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"headless":true,"idle":"1s"}`), 0o600)
			_ = os.WriteFile(filepath.Join(dir, "last_used"), nil, 0o600)
			old := time.Now().Add(-time.Hour)
			_ = os.Chtimes(filepath.Join(dir, "last_used"), old, old)

			oko(t, "eval", "1") // sweeps other profiles
			must(t, oko(t, "-p", name, "status"), "stopped")
			oko(t, "-p", name, "down")
			mustLive(t, exited, "a process oko did not launch")
			if _, err := os.Stat(filepath.Join(dir, "idle.proc")); err == nil {
				t.Fatal("a reaper was started for a process that is not the profile's Chrome")
			}
		})
	}
}

// A real oko relay of another home, named by this home's stale records (the
// old state.json field, or a record whose start time does not match): 'oko
// down' here must leave it alone. Same profile name, different home.
func TestHelperOfOtherHomeSignalsNothing(t *testing.T) {
	homeB := t.TempDir()
	c := exec.Command(bin, "_proxy", "--profile", "work")
	c.Env = append(os.Environ(), "OKO_HOME="+homeB)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	defer c.Process.Kill()
	time.Sleep(300 * time.Millisecond)
	relayB := c.Process.Pid

	dir := filepath.Join(home, "profiles", "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(fmt.Sprintf(`{"headless":true,"relay":%d}`, relayB)), 0o600)
	canon, _ := filepath.EvalSymlinks(home)
	exe, _ := filepath.EvalSymlinks(bin)
	rec := fmt.Sprintf(`{"pid":%d,"start":"1.000000","exe":%q,"home":%q}`, relayB, exe, canon)
	_ = os.WriteFile(filepath.Join(dir, "relay.proc"), []byte(rec), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "watch.proc"), []byte(rec), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "idle.proc"), []byte(rec), 0o600)
	oko(t, "-p", "work", "down")
	mustLive(t, exited, "home B's relay")
}

// Valid records of one profile copied into another profile of the same home
// prove nothing there: 'down' on the copy leaves the original's Chrome and
// reaper running.
func TestCopiedRecordsSignalNothing(t *testing.T) {
	orig := []string{"-p", "recorig"}
	defer oko(t, append(orig, "down")...)
	must(t, oko(t, append(orig, "up", "--headless", "--idle", "30m")...), "started")
	src := filepath.Join(home, "profiles", "recorig")
	var reaper int
	for i := 0; i < 30 && reaper == 0; i++ {
		reaper = procPID(filepath.Join(src, "idle.proc"))
		time.Sleep(100 * time.Millisecond)
	}
	chromePID := procPID(filepath.Join(src, "chrome.proc"))
	if chromePID == 0 || reaper == 0 {
		t.Fatalf("no records: chrome %d, reaper %d", chromePID, reaper)
	}
	dst := filepath.Join(home, "profiles", "reccopy")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"chrome.proc", "idle.proc"} {
		b, _ := os.ReadFile(filepath.Join(src, f))
		_ = os.WriteFile(filepath.Join(dst, f), b, 0o600)
	}
	// Same purpose swapped too: the reaper's record as the watcher's.
	b, _ := os.ReadFile(filepath.Join(src, "idle.proc"))
	_ = os.WriteFile(filepath.Join(dst, "watch.proc"), b, 0o600)
	must(t, oko(t, "-p", "reccopy", "down"), "not running")
	time.Sleep(time.Second)
	if syscall.Kill(chromePID, 0) != nil || syscall.Kill(reaper, 0) != nil {
		t.Fatal("a copied record signalled the original profile's processes")
	}
	must(t, oko(t, append(orig, "status")...), "running")
}

// recorderOf returns the tracked recorder pid and capture dir of a profile's
// only recording.
func recorderOf(t *testing.T, profile string) (int, string) {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "profiles", profile, "state.json"))
	var st struct {
		Recorders map[string]int `json:"recorders"`
	}
	_ = json.Unmarshal(b, &st)
	if len(st.Recorders) != 1 {
		t.Fatalf("want one tracked recorder, got %v", st.Recorders)
	}
	dirs, _ := filepath.Glob(filepath.Join(home, "profiles", profile, "recording-*"))
	if len(dirs) != 1 {
		t.Fatalf("want one capture dir, got %v", dirs)
	}
	for _, pid := range st.Recorders {
		return pid, dirs[0]
	}
	return 0, ""
}

// 'record stop' stops only a recorder it can prove; a live one it cannot
// (an older oko's: no record) keeps running, tracked, with its frames, and
// stop says so. Once that recorder is gone, stop encodes what it captured.
func TestRecordStopUnprovenRecorder(t *testing.T) {
	p := []string{"-p", "recunproven"}
	defer oko(t, append(p, "down")...)
	oko(t, append(p, "up", "--headless", "--idle", "off")...)
	oko(t, append(p, "open", base+"/form.html")...)
	oko(t, append(p, "record", "start")...)
	oko(t, append(p, "eval", "document.body.style.background = 'red'")...)
	time.Sleep(500 * time.Millisecond)
	pid, dir := recorderOf(t, "recunproven")
	// As if an older oko had started it: no process record.
	if err := os.Remove(filepath.Join(dir, "recorder.proc")); err != nil {
		t.Fatal(err)
	}
	out := oko(t, append([]string{"!"}, append(p, "record", "stop", "-o", filepath.Join(t.TempDir(), "x.gif"))...)...)
	must(t, out, "cannot prove", "nothing was stopped")
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("unproven recorder was signalled")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("frames removed")
	}
	if p2, _ := recorderOf(t, "recunproven"); p2 != pid {
		t.Fatal("recorder no longer tracked")
	}
	// The user ends it (the test owns this process); stop now encodes.
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	gif := filepath.Join(t.TempDir(), "y.gif")
	must(t, oko(t, append(p, "record", "stop", "-o", gif)...), "frames")
	if _, err := os.Stat(gif); err != nil {
		t.Fatal("no gif after the recorder was gone")
	}
}

// A tracked recorder pid that now belongs to another process: not
// signalled; --forget drops the tracking and leaves the frames.
func TestRecordStopReusedPID(t *testing.T) {
	p := []string{"-p", "recreused"}
	defer oko(t, append(p, "down")...)
	oko(t, append(p, "up", "--headless", "--idle", "off")...)
	oko(t, append(p, "open", base+"/form.html")...)
	oko(t, append(p, "record", "start")...)
	pid, dir := recorderOf(t, "recreused")
	// The real recorder ends; its pid "is reused" by a test process.
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	victim, exited := startVictim(t, "sleep", "60")
	sp := filepath.Join(home, "profiles", "recreused", "state.json")
	b, _ := os.ReadFile(sp)
	_ = os.WriteFile(sp, []byte(strings.Replace(string(b), strconv.Itoa(pid), strconv.Itoa(victim), 1)), 0o600)
	must(t, oko(t, append([]string{"!"}, append(p, "record", "stop")...)...), "cannot prove")
	must(t, oko(t, append(p, "record", "stop", "--forget")...), "stopped tracking", "not signalled")
	mustLive(t, exited, "process with the reused pid")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("--forget removed the frames")
	}
	must(t, oko(t, append([]string{"!"}, append(p, "record", "stop")...)...), "not recording")
}
