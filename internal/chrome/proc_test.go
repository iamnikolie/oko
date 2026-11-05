package chrome

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// spawn starts a throwaway process the test owns; done reports its exit.
func spawn(t *testing.T, name string, args ...string) (pid int, done <-chan error) {
	t.Helper()
	c := exec.Command(name, args...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ch := make(chan error, 1)
	go func() { ch <- c.Wait() }()
	t.Cleanup(func() { _ = c.Process.Kill() })
	time.Sleep(100 * time.Millisecond) // let exec settle
	return c.Process.Pid, ch
}

func alive(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s was signalled: %v", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

func sleepBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	return p
}

func lockTo(t *testing.T, p *Profile, pid int) {
	t.Helper()
	if err := os.MkdirAll(p.UserDataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	lock := filepath.Join(p.UserDataDir(), "SingletonLock")
	_ = os.Remove(lock)
	if err := os.Symlink(host+"-"+strconv.Itoa(pid), lock); err != nil {
		t.Fatal(err)
	}
}

func needIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no process identity on " + runtime.GOOS)
	}
}

// A record oko writes for a process it started proves that process, and
// only while it is the same process.
func TestRecordProvesIdentity(t *testing.T) {
	needIdentity(t)
	t.Setenv("OKO_HOME", t.TempDir())
	bin := sleepBin(t)
	before := time.Now()
	pid, done := spawn(t, bin, "60")
	path := filepath.Join(Home(), "x.proc")
	if err := RecordProc(path, pid, bin, before); err != nil {
		t.Fatal(err)
	}
	if got := OwnedPID(path); got != pid {
		t.Fatalf("OwnedPID = %d, want %d", got, pid)
	}
	// The wrong executable or a launch after the process started: no record.
	if err := RecordProc(path+"2", pid, "/bin/sh", before); err == nil {
		t.Fatal("recorded a process that runs another executable")
	}
	if err := RecordProc(path+"3", pid, bin, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("recorded a process that started before the launch")
	}
	if !SignalOwned(path, syscall.SIGTERM) {
		t.Fatal("own process not signalled")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("own process did not get the signal")
	}
	if OwnedPID(path) != 0 {
		t.Fatal("a dead process is still owned")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("stale record not cleared")
	}
}

// A record whose pid now belongs to another process (other start time) is
// cleared, and that process is never signalled.
func TestStaleRecordSignalsNothing(t *testing.T) {
	needIdentity(t)
	t.Setenv("OKO_HOME", t.TempDir())
	bin := sleepBin(t)
	pid, done := spawn(t, bin, "60")
	path := filepath.Join(Home(), "x.proc")
	b, _ := json.Marshal(procRecord{PID: pid, Start: "1.000000", Exe: resolveExe(bin), Home: canonicalHome()})
	_ = os.WriteFile(path, b, 0o600)
	if SignalOwned(path, syscall.SIGTERM) {
		t.Fatal("signalled a process with another start time")
	}
	alive(t, done, "reused pid")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("stale record not cleared")
	}
}

// A non-Chrome process whose arguments match an oko Chrome exactly, named
// by the profile's lock: never the profile's Chrome, never signalled.
func TestNonChromeWithMatchingArgsNotOwned(t *testing.T) {
	needIdentity(t)
	t.Setenv("OKO_HOME", t.TempDir())
	p, _ := Load("stale")
	pid, done := spawn(t, "sh", "-c", "sleep 60; :", "chrome",
		"--user-data-dir="+p.UserDataDir(), "--remote-debugging-port=0")
	lockTo(t, p, pid)
	if got := p.PID(); got != 0 {
		t.Fatalf("PID() = %d for a process oko never launched", got)
	}
	// Even a launch record attempt refuses it: it does not run Chrome.
	p.recordLaunch("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", time.Now().Add(-time.Minute))
	if got := p.PID(); got != 0 {
		t.Fatalf("PID() = %d after recording a non-Chrome process", got)
	}
	if p.SignalChrome(syscall.SIGKILL) {
		t.Fatal("signalled a non-Chrome process")
	}
	alive(t, done, "non-Chrome process with matching args")
	if p.LockHolder() != pid {
		t.Fatal("LockHolder should still observe the lock")
	}
}

// A genuine record from another oko home (a copied profile dir, or the same
// profile name under another home) is not this home's to signal.
func TestRecordFromOtherHomeNotOwned(t *testing.T) {
	needIdentity(t)
	bin := sleepBin(t)
	homeB := t.TempDir()
	t.Setenv("OKO_HOME", homeB)
	pid, done := spawn(t, bin, "60")
	recB := filepath.Join(homeB, "relay.proc")
	if err := RecordProc(recB, pid, bin, time.Time{}); err != nil {
		t.Fatal(err)
	}
	homeA := t.TempDir()
	t.Setenv("OKO_HOME", homeA)
	recA := filepath.Join(homeA, "relay.proc")
	b, _ := os.ReadFile(recB)
	_ = os.WriteFile(recA, b, 0o600)
	if OwnedPID(recA) != 0 || SignalOwned(recA, syscall.SIGTERM) {
		t.Fatal("home A signalled home B's process")
	}
	alive(t, done, "home B's helper")
	t.Setenv("OKO_HOME", homeB)
	if OwnedPID(recB) != pid {
		t.Fatal("home B no longer owns its own process")
	}
}

// Concurrent commands applying different limits under shared leases leave
// one whole valid value; readers never see a partial one.
func TestConcurrentApplyIdle(t *testing.T) {
	t.Setenv("OKO_HOME", t.TempDir())
	a, _ := Load("lim")
	b, _ := Load("lim")
	a.State.Idle, b.State.Idle = "2h", "off"
	ctx := t.Context()
	ra, err := a.Use(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ra()
	rb, err := b.Use(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rb()
	stop := make(chan struct{})
	var bad []string
	var mu sync.Mutex
	var rd sync.WaitGroup
	rd.Add(1)
	go func() {
		defer rd.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if v, err := os.ReadFile(a.idleLimitPath()); err == nil {
				if _, ok := ParseIdle(string(v)); !ok {
					mu.Lock()
					bad = append(bad, string(v))
					mu.Unlock()
				}
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if err := writeAtomic(a.idleLimitPath(), []byte("3s")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = a.ApplyIdle() }()
		go func() { defer wg.Done(); _ = b.ApplyIdle() }()
		wg.Wait()
		v, _ := os.ReadFile(a.idleLimitPath())
		if s := string(v); s != "2h0m0s" && s != "off" {
			t.Fatalf("iteration %d: idle.limit = %q", i, s)
		}
		if got := a.IdleLimit(); got != b.IdleLimit() {
			t.Fatalf("iteration %d: handles disagree: %v vs %v", i, got, b.IdleLimit())
		}
	}
	close(stop)
	rd.Wait()
	if len(bad) > 0 {
		t.Fatalf("readers saw partial values: %q", bad[:min(5, len(bad))])
	}
	if m, _ := filepath.Glob(filepath.Join(a.Dir, "idle.limit.*")); len(m) > 0 {
		t.Fatalf("temp files left behind: %v", m)
	}
}

// The applied idle limit is what every process reads, whatever its own
// environment says.
func TestIdleLimitApplied(t *testing.T) {
	t.Setenv("OKO_HOME", t.TempDir())
	p, _ := Load("lim")
	t.Setenv("OKO_IDLE", "2s")
	_ = p.ApplyIdle()
	t.Setenv("OKO_IDLE", "off")
	if got := p.IdleLimit(); got != 2*time.Second {
		t.Fatalf("IdleLimit = %v, want the applied 2s", got)
	}
	_ = p.ApplyIdle()
	if got := p.IdleLimit(); got != 0 {
		t.Fatalf("IdleLimit = %v after applying off", got)
	}
	p.ClearIdle()
	t.Setenv("OKO_IDLE", "")
	if got := p.IdleLimit(); got != DefaultIdle {
		t.Fatalf("IdleLimit = %v, want default", got)
	}
}
