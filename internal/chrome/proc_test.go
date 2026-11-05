package chrome

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// spawn starts a throwaway process the test owns and returns its pid.
func spawn(t *testing.T, name string, args ...string) int {
	t.Helper()
	c := exec.Command(name, args...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	// ps sees the final command line once the exec settled.
	time.Sleep(100 * time.Millisecond)
	return c.Process.Pid
}

func lockTo(t *testing.T, p *Profile, host string, pid int) {
	t.Helper()
	if err := os.MkdirAll(p.UserDataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(p.UserDataDir(), "SingletonLock")
	_ = os.Remove(lock)
	if err := os.Symlink(host+"-"+strconv.Itoa(pid), lock); err != nil {
		t.Fatal(err)
	}
}

// A stale SingletonLock whose pid now belongs to another process must not
// make that process "the profile's Chrome".
func TestPIDNeedsProvenChrome(t *testing.T) {
	t.Setenv("OKO_HOME", t.TempDir())
	p, err := Load("stale")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()

	other := spawn(t, "sleep", "60")
	lockTo(t, p, host, other)
	if got := p.PID(); got != 0 {
		t.Fatalf("unrelated process %d taken for the profile's Chrome", got)
	}

	// Same command line as an oko Chrome, but the lock was written on
	// another host: the pid means nothing here.
	fake := spawn(t, "sh", "-c", "sleep 60; :", "chrome", "--user-data-dir="+p.UserDataDir(), "--remote-debugging-port=0")
	lockTo(t, p, "elsewhere.local", fake)
	if got := p.PID(); got != 0 {
		t.Fatalf("lock from another host accepted: %d", got)
	}

	// Right host and the profile's own arguments: ours.
	lockTo(t, p, host, fake)
	if got := p.PID(); got != fake {
		t.Fatalf("PID() = %d, want %d", got, fake)
	}
	// Another profile's dir is not ours (whole-argument match).
	q, _ := Load("stale2")
	lockTo(t, q, host, fake)
	if got := q.PID(); got != 0 {
		t.Fatalf("profile stale2 claimed stale's Chrome: %d", got)
	}

	// A recorded launch of this pid with another start time: pid reused.
	p.recordLaunch()
	if got := p.PID(); got != fake {
		t.Fatalf("after recordLaunch PID() = %d, want %d", got, fake)
	}
	if err := os.WriteFile(p.launchIDPath(), []byte(strconv.Itoa(fake)+"\nMon Jan  1 00:00:00 2001"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.PID(); got != 0 {
		t.Fatalf("pid with another start time accepted: %d", got)
	}
}

func TestHelperIdentity(t *testing.T) {
	pid := spawn(t, "sh", "-c", "sleep 60; :", "oko", "_idle", "--profile", "work")
	if Helper(pid, "_idle", "work") != pid {
		t.Fatal("own reaper not recognised")
	}
	for _, c := range [][2]string{{"_idle", "wor"}, {"_watch", "work"}, {"_idle", "work2"}} {
		if Helper(pid, c[0], c[1]) != 0 {
			t.Fatalf("Helper(%s, %s) matched a different process", c[0], c[1])
		}
	}
	if Helper(spawn(t, "sleep", "60"), "_idle", "work") != 0 {
		t.Fatal("unrelated process taken for a reaper")
	}
}

// The applied idle limit is what every process reads, whatever its own
// environment says.
func TestIdleLimitApplied(t *testing.T) {
	t.Setenv("OKO_HOME", t.TempDir())
	p, _ := Load("lim")
	t.Setenv("OKO_IDLE", "2s")
	p.ApplyIdle()
	t.Setenv("OKO_IDLE", "off")
	if got := p.IdleLimit(); got != 2*time.Second {
		t.Fatalf("IdleLimit = %v, want the applied 2s", got)
	}
	p.ApplyIdle()
	if got := p.IdleLimit(); got != 0 {
		t.Fatalf("IdleLimit = %v after applying off", got)
	}
	p.ClearIdle()
	t.Setenv("OKO_IDLE", "")
	if got := p.IdleLimit(); got != DefaultIdle {
		t.Fatalf("IdleLimit = %v, want default", got)
	}
}
