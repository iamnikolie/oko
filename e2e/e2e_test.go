// Package e2e runs the real oko binary against a headless Chrome and local
// fixture pages. It needs Chrome (or $OKO_CHROME); without it the tests skip.
//
//	go test ./e2e/          # ~30 s
package e2e

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/oko/internal/chrome"
)

var (
	bin  string // built oko binary
	home string // isolated OKO_HOME
	base string // http://localhost:PORT
	alt  string // http://127.0.0.1:PORT2 — a different site, for cross-origin frames
)

func TestMain(m *testing.M) {
	if _, err := chrome.ChromePath(); err != nil {
		fmt.Println("skip e2e: no Chrome")
		os.Exit(0)
	}
	tmp, err := os.MkdirTemp("", "oko-e2e-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(tmp, "oko")
	build := exec.Command("go", "build", "-o", bin, "..")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	home = filepath.Join(tmp, "home")
	os.Setenv("OKO_HOME", home)
	os.Setenv("OKO_PROFILE", "default")
	os.Setenv("OKO_HUMAN", "")
	os.Setenv("OKO_TAB", "")
	os.Setenv("OKO_SESSION", "")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	os.Unsetenv("CODEX_THREAD_ID")

	base = serve("localhost", http.FileServer(http.Dir("testdata")))
	alt = serve("127.0.0.1", http.FileServer(http.Dir("testdata")))

	if out, err := exec.Command(bin, "up", "--headless").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = exec.Command(bin, "down").Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func serve(host string, h http.Handler) string {
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		panic(err)
	}
	go func() { _ = http.Serve(ln, h) }()
	return "http://" + host + ":" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// oko runs the binary and returns combined output; it fails the test on a
// non-zero exit unless the args start with "!".
func oko(t *testing.T, args ...string) string {
	t.Helper()
	expectFail := len(args) > 0 && args[0] == "!"
	if expectFail {
		args = args[1:]
	}
	c := exec.Command(bin, args...)
	out, err := c.CombinedOutput()
	s := string(out)
	if expectFail {
		if err == nil {
			t.Fatalf("oko %v: expected failure, got:\n%s", args, s)
		}
		return s
	}
	if err != nil {
		t.Fatalf("oko %v: %v\n%s", args, err, s)
	}
	return s
}

func must(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("output lacks %q:\n%s", w, out)
		}
	}
}

func mustNot(t *testing.T, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Fatalf("output unexpectedly contains %q:\n%s", b, out)
		}
	}
}

// ref returns the ref of the first snapshot line containing needle.
func ref(t *testing.T, snap, needle string) string {
	t.Helper()
	for _, l := range strings.Split(snap, "\n") {
		if strings.Contains(l, needle) && strings.HasPrefix(l, "[") {
			return l[1:strings.Index(l, "]")]
		}
	}
	t.Fatalf("no element %q in snapshot:\n%s", needle, snap)
	return ""
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

// cmdWithStdin runs oko with stdin and returns stdout only.
func cmdWithStdin(stdin string, args ...string) string {
	c := exec.Command(bin, args...)
	c.Stdin = strings.NewReader(stdin)
	out, _ := c.Output()
	return string(out)
}
