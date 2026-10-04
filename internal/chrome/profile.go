// Package chrome owns the agent browser: one dedicated Chrome per profile,
// its user-data dir, its debugging port, and launching/stopping it.
package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// State is persisted per profile in state.json.
type State struct {
	// Port is the last seen debugging port (informational). The current tab
	// is per caller, in sessions/ (see Session).
	Port     int  `json:"port,omitempty"`
	Headless bool `json:"headless,omitempty"`
	// Lang is the browser UI and Accept-Language locale, e.g. "en-US".
	Lang string `json:"lang,omitempty"`
	// Proxy is the upstream proxy URL (credentials included) Chrome reaches
	// the web through, via the local relay; empty means direct.
	Proxy string `json:"proxy,omitempty"`
	// Relay is the pid of the profile's proxy relay.
	Relay int `json:"relay,omitempty"`
	// Human makes input human-like by default (curved mouse, wheel scroll).
	Human bool `json:"human,omitempty"`
	// Mouse is the last pointer position oko produced, per tab.
	Mouse map[string][2]float64 `json:"mouse,omitempty"`
	// Recorders maps tab id to the pid of its screencast recorder.
	Recorders map[string]int `json:"recorders,omitempty"`
	// Seq is the last snapshot ref number handed out per tab.
	Seq map[string]int `json:"seq,omitempty"`
	// Holders maps tab id to the pid keeping its device emulation alive.
	Holders map[string]int `json:"holders,omitempty"`
	// NoWatch keeps the element-picker watcher from starting with the browser.
	NoWatch bool `json:"no_watch,omitempty"`
}

type Profile struct {
	Name  string
	Dir   string
	State State
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Home is the oko root: $OKO_HOME or ~/.oko.
func Home() string {
	if v := os.Getenv("OKO_HOME"); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".oko"
	}
	return filepath.Join(h, ".oko")
}

// Load opens (creating if needed) the named profile and assigns it a port.
func Load(name string) (*Profile, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid profile name %q (letters, digits, - and _ only)", name)
	}
	p := &Profile{Name: name, Dir: filepath.Join(Home(), "profiles", name)}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(p.statePath()); err == nil {
		_ = json.Unmarshal(b, &p.State)
	}
	return p, nil
}

func (p *Profile) statePath() string   { return filepath.Join(p.Dir, "state.json") }
func (p *Profile) UserDataDir() string { return filepath.Join(p.Dir, "chrome") }
func (p *Profile) LogPath() string     { return filepath.Join(p.Dir, "chrome.log") }

func (p *Profile) Save() error {
	b, _ := json.MarshalIndent(p.State, "", "  ")
	tmp := p.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.statePath())
}

// Endpoint returns the websocket URL of this profile's own Chrome, or an
// error when it is not running. Chrome is started with a random port and
// writes it to DevToolsActivePort inside the user-data dir; requiring the
// browser on that port to report the same websocket path means oko never
// attaches to some other Chrome that happens to listen on a known port.
func (p *Profile) Endpoint(ctx context.Context) (string, error) {
	b, err := os.ReadFile(filepath.Join(p.UserDataDir(), "DevToolsActivePort"))
	if err != nil {
		return "", errors.New("not running")
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return "", errors.New("bad DevToolsActivePort")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return "", errors.New("bad DevToolsActivePort")
	}
	path := strings.TrimSpace(lines[1])

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if !strings.HasSuffix(v.WS, path) {
		return "", errors.New("port now belongs to another browser")
	}
	if p.State.Port != port {
		p.State.Port = port
		_ = p.Save()
	}
	return v.WS, nil
}

// PID returns the process id holding this profile's SingletonLock when that
// process is alive. Chrome's lock is a symlink to "<host>-<pid>".
func (p *Profile) PID() int {
	target, err := os.Readlink(filepath.Join(p.UserDataDir(), "SingletonLock"))
	if err != nil {
		return 0
	}
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return 0
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return 0
	}
	if syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

func frontmostPID() int {
	out, err := exec.Command("osascript", "-e",
		`tell application "System Events" to get unix id of first application process whose frontmost is true`).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func refocus(pid int) {
	_ = exec.Command("osascript", "-e", fmt.Sprintf(
		`tell application "System Events" to set frontmost of (first application process whose unix id is %d) to true`, pid)).Run()
}

// appBundle maps ".../X.app/Contents/MacOS/X" to ".../X.app" for open(1).
func appBundle(bin string) string {
	if i := strings.Index(bin, ".app/"); i > 0 {
		return bin[:i+4]
	}
	return bin
}

// ChromePath finds the browser binary: $OKO_CHROME, then the usual install
// locations.
func ChromePath() (string, error) {
	if v := os.Getenv("OKO_CHROME"); v != "" {
		return v, nil
	}
	for _, c := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	for _, c := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", errors.New("chrome not found; set OKO_CHROME to the browser binary")
}

// Launch starts a detached Chrome for this profile and waits until its
// debugging endpoint answers. The process outlives oko.
func (p *Profile) Launch(ctx context.Context, headless bool) (string, error) {
	bin, err := ChromePath()
	if err != nil {
		return "", err
	}
	args := []string{
		"--user-data-dir=" + p.UserDataDir(),
		"--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,MediaRouter",
		// Background tabs otherwise freeze timers and rendering, which
		// stalls screenshots and waits on a tab the agent is not looking at.
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
	}
	if headless {
		args = append(args, "--headless=new", "--window-size=1440,900")
	}
	if p.State.Lang != "" {
		base := strings.SplitN(p.State.Lang, "-", 2)[0]
		args = append(args, "--lang="+p.State.Lang, "--accept-lang="+p.State.Lang+","+base)
	}
	if p.State.Proxy != "" {
		port, err := p.ensureRelay()
		if err != nil {
			return "", err
		}
		// WebRTC would otherwise reach STUN servers around the proxy and
		// reveal the real address.
		args = append(args, fmt.Sprintf("--proxy-server=http://127.0.0.1:%d", port),
			"--force-webrtc-ip-handling-policy=disable_non_proxied_udp")
	}
	args = append(args, "about:blank")

	_ = os.Remove(filepath.Join(p.UserDataDir(), "DevToolsActivePort"))
	logf, err := os.OpenFile(p.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer logf.Close()
	c := exec.Command(bin, args...)
	viaOpen := runtime.GOOS == "darwin" && !headless
	front := 0
	if viaOpen {
		front = frontmostPID()
		// Start behind the frontmost app instead of stealing focus.
		c = exec.Command("open", append([]string{"-g", "-n", "-a", appBundle(bin), "--args"}, args...)...)
	}
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return "", fmt.Errorf("start chrome: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()

	p.State.Headless = headless
	_ = p.Save()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ws, err := p.Endpoint(ctx); err == nil {
			if front > 0 {
				// Chrome raises its new window anyway; hand focus back.
				time.Sleep(300 * time.Millisecond)
				refocus(front)
			}
			return ws, nil
		}
		select {
		case err := <-exited:
			if viaOpen && err == nil {
				exited = nil // open(1) returns once Chrome is launched
				continue
			}
			return "", fmt.Errorf("chrome exited during start (%v); see %s", err, p.LogPath())
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("chrome did not open its debugging port within 20s; see %s (is this profile open in another Chrome window?)", p.LogPath())
}
