package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
)

// extChrome is the browser for the extension test: $OKO_EXT_CHROME, else the
// Chrome oko uses. It always runs headless on a throwaway user-data dir with
// quietChromeFlags, never on the user's profile. The extension goes in through CDP Extensions.loadUnpacked
// (branded Chrome ignores --load-extension since 137).
func extChrome() string {
	if v := os.Getenv("OKO_EXT_CHROME"); v != "" {
		return v
	}
	p, _ := chrome.ChromePath()
	return p
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// extHarness is a throwaway Chrome with the unpacked extension loaded; the
// test plays the user by calling the extension's worker (what the popup does).
type extHarness struct {
	t   *testing.T
	b   *rod.Browser
	sw  string // session on the extension's worker
	cmd *exec.Cmd
}

func startExtChrome(t *testing.T, extDir string) *extHarness {
	bin := extChrome()
	if bin == "" {
		t.Skip("no Chrome")
	}
	udd := t.TempDir()
	// A pipe (fds 3 and 4) next to the port: Extensions.loadUnpacked is only
	// served over --remote-debugging-pipe.
	toChrome, chromeIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	chromeOut, fromChrome, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"--user-data-dir=" + udd, "--headless=new", "--remote-debugging-port=0",
		"--remote-debugging-pipe", "--enable-unsafe-extension-debugging", "--window-size=1280,900"}, quietChromeFlags...)
	cmd := exec.Command(bin, append(args, "about:blank")...)
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	// Its own process group, so teardown reaches the helper processes too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = toChrome.Close()
	_ = fromChrome.Close()
	// Registered before anything can fail, and after t.TempDir, so it runs
	// before the profile is removed.
	t.Cleanup(func() { stopChrome(t, cmd, chromeIn, udd); _ = chromeOut.Close() })
	if _, err := chromeIn.Write([]byte(fmt.Sprintf(`{"id":1,"method":"Extensions.loadUnpacked","params":{"path":%q}}`, extDir) + "\x00")); err != nil {
		t.Fatal(err)
	}
	reply := make(chan string, 1)
	go func() {
		r := bufio.NewReader(chromeOut)
		for {
			msg, err := r.ReadString(0)
			if err != nil {
				return
			}
			if strings.Contains(msg, `"id":1`) {
				reply <- msg
			}
		}
	}()
	select {
	case msg := <-reply:
		if !strings.Contains(msg, `"result"`) {
			t.Fatalf("Extensions.loadUnpacked: %s", msg)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Extensions.loadUnpacked: no answer")
	}
	h := &extHarness{t: t, cmd: cmd}
	var ws string
	for i := 0; i < 100 && ws == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		if b, err := os.ReadFile(filepath.Join(udd, "DevToolsActivePort")); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) == 2 {
				ws = "ws://127.0.0.1:" + lines[0] + lines[1]
			}
		}
	}
	if ws == "" {
		t.Fatal("test chrome did not start")
	}
	h.b = rod.New().ControlURL(ws)
	if err := h.b.Connect(); err != nil {
		t.Fatal(err)
	}
	var sw proto.TargetTargetID
	for i := 0; i < 100 && sw == ""; i++ {
		res, err := proto.TargetGetTargets{}.Call(h.b)
		if err == nil {
			for _, ti := range res.TargetInfos {
				if ti.Type == "service_worker" && strings.HasPrefix(ti.URL, "chrome-extension://") && strings.HasSuffix(ti.URL, "/background.js") {
					sw = ti.TargetID
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if sw == "" {
		t.Fatal("extension worker not found: the extension did not load")
	}
	sess, err := proto.TargetAttachToTarget{TargetID: sw, Flatten: true}.Call(h.b)
	if err != nil {
		t.Fatal(err)
	}
	h.sw = string(sess.SessionID)
	_, _ = h.b.Call(context.Background(), h.sw, "Runtime.runIfWaitingForDebugger", nil)
	var last string
	for i := 0; i < 100; i++ {
		v, err := h.js("typeof globalThis.oko")
		last = fmt.Sprint(v, err)
		if v == `"object"` {
			return h
		}
		time.Sleep(100 * time.Millisecond)
	}
	where, _ := h.js("location.href + ' ' + Object.keys(globalThis).filter(k => !/^on/.test(k)).slice(0,30).join(',')")
	t.Fatalf("extension worker never came up: %s (%s)", last, where)
	return h
}

// stopChrome closes the test Chrome the way a user would (Browser.close over
// the pipe), waits for it, then kills whatever is left of its process group
// and empties the profile while no process can write to it any more. A plain
// kill left helper processes writing into the profile while the test's
// TempDir cleanup removed it ("directory not empty").
func stopChrome(t *testing.T, cmd *exec.Cmd, pipe *os.File, udd string) {
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	_, _ = pipe.Write([]byte(`{"id":999999,"method":"Browser.close"}` + "\x00"))
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	_ = pipe.Close()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	var err error
	for i := 0; i < 50; i++ {
		if err = os.RemoveAll(udd); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("test Chrome profile not removed: %v", err)
}

// js runs an expression in the extension's worker and returns its JSON value.
func (h *extHarness) js(expr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := h.b.Call(ctx, h.sw, "Runtime.evaluate", proto.RuntimeEvaluate{
		Expression: "(async () => JSON.stringify(await (" + expr + ")))()", AwaitPromise: true, ReturnByValue: true})
	if err != nil {
		return "", err
	}
	var res proto.RuntimeEvaluateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	if res.ExceptionDetails != nil {
		msg := res.ExceptionDetails.Text
		if res.ExceptionDetails.Exception != nil {
			msg = res.ExceptionDetails.Exception.Description
		}
		return "", fmt.Errorf("%s", msg)
	}
	return res.Result.Value.Str(), nil
}

func (h *extHarness) mustJS(expr string) string {
	h.t.Helper()
	v, err := h.js(expr)
	if err != nil {
		h.t.Fatalf("%s: %v", expr, err)
	}
	return v
}

// openTab opens a tab the way a user would (not through oko) and returns its
// Chrome tab id.
func (h *extHarness) openTab(url string) int {
	h.t.Helper()
	v := h.mustJS(fmt.Sprintf(`(async () => { const t = await chrome.tabs.create({url: %q, active: true});
		for (let i = 0; i < 100; i++) { const x = await chrome.tabs.get(t.id); if (x.status === 'complete') break; await new Promise(r => setTimeout(r, 50)); }
		return t.id; })()`, url))
	n, err := strconv.Atoi(v)
	if err != nil {
		h.t.Fatalf("tab id %q", v)
	}
	return n
}

func TestExtensionProfile(t *testing.T) {
	port := freePort(t)
	t.Setenv("OKO_EXT_PORT", strconv.Itoa(port))
	t.Setenv("OKO_PROFILE", "")
	defer func() { _ = exec.Command(bin, "ext", "stop").Run() }()

	extDir := strings.TrimSpace(oko(t, "ext", "path"))
	h := startExtChrome(t, extDir)

	// Pairing: oko prints a code, the user types it into the popup.
	out := oko(t, "ext", "pair", "--no-wait")
	code := regexp.MustCompile(`pairing code: (\S+)`).FindStringSubmatch(out)
	if code == nil {
		t.Fatalf("no code in:\n%s", out)
	}
	h.mustJS(fmt.Sprintf("oko.setPort(%d)", port))
	if _, err := h.js(`oko.pair("WRONG-CODE")`); err == nil || !strings.Contains(err.Error(), "did not prove the pairing code") {
		lg, _ := os.ReadFile(filepath.Join(home, "extension", "bridge.log"))
		st, _ := h.js("oko.state()")
		t.Fatalf("wrong code accepted: %v\nstate: %s\nbridge log:\n%s", err, st, lg)
	}
	h.mustJS(fmt.Sprintf("oko.pair(%q)", strings.ToLower(code[1])))
	must(t, oko(t, "ext", "status"), "paired with chrome-extension://", "extension connected", "tabs handed to oko: none")
	// The code works once.
	if _, err := h.js(fmt.Sprintf("oko.pair(%q)", code[1])); err == nil {
		t.Fatal("pairing code worked twice")
	}
	h.mustJS("oko.connect()")
	for i := 0; i < 50 && !strings.Contains(oko(t, "ext", "status"), "extension connected"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if st := oko(t, "ext", "status"); !strings.Contains(st, "extension connected") {
		lg, _ := os.ReadFile(filepath.Join(home, "extension", "bridge.log"))
		t.Fatalf("no reconnect: %s\n%s\n%s", st, h.mustJS("oko.state()"), lg)
	}

	// Nothing is handed over: oko sees no tab and may not open one.
	other := h.openTab(base + "/article.html")
	mustNot(t, oko(t, "-p", "mine", "tabs"), "Article")
	must(t, oko(t, "!", "-p", "mine", "snap"), "no tab is handed to oko")

	// Hand over a tab: the main commands work on it unchanged.
	form := h.openTab(base + "/form.html")
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", form))
	if got := h.mustJS(fmt.Sprintf(`Promise.all([chrome.action.getTitle({tabId: %d}), chrome.action.getBadgeText({}), chrome.action.getTitle({})]).then((v) => v.join("|"))`, form)); got != `"oko controls this tab||oko"` {
		t.Fatalf("toolbar for a held tab: %s", got)
	}
	waitFor(t, func() bool { return strings.Contains(oko(t, "ext", "status"), "tabs handed to oko: 1") })
	tabs := oko(t, "-p", "mine", "tabs")
	must(t, tabs, "Form")
	mustNot(t, tabs, "Article")

	s := oko(t, "-p", "mine", "snap")
	must(t, s, `textbox "Email"`, `button "Send"`)
	oko(t, "-p", "mine", "fill", ref(t, s, `textbox "Email"`), "me@x.io")
	oko(t, "-p", "mine", "fill", ref(t, s, "combobox"), "Pro")
	oko(t, "-p", "mine", "click", ref(t, s, `checkbox "I agree"`))
	oko(t, "-p", "mine", "click", ref(t, s, `button "Send"`))
	must(t, oko(t, "-p", "mine", "text", "#out"), "sent me@x.io p true")
	must(t, oko(t, "-p", "mine", "eval", "document.title"), "Form")
	must(t, oko(t, "-p", "mine", "html", "#out"), `<p id="out">`)
	oko(t, "-p", "mine", "hover", "text=Confirm me")
	oko(t, "-p", "mine", "click", "#notes")
	oko(t, "-p", "mine", "type", "typed")
	oko(t, "-p", "mine", "press", "Enter")
	must(t, oko(t, "-p", "mine", "eval", "document.getElementById('notes').textContent"), "typed")
	oko(t, "-p", "mine", "scroll")
	must(t, oko(t, "-p", "mine", "click", "text=Confirm me"), "accepted")
	shot := strings.TrimSpace(oko(t, "-p", "mine", "shot"))
	if fi, err := os.Stat(strings.Fields(shot)[0]); err != nil || fi.Size() < 1000 {
		t.Fatalf("shot: %v %s", err, shot)
	}

	oko(t, "-p", "mine", "open", base+"/article.html")
	must(t, oko(t, "-p", "mine", "read"), "#")
	oko(t, "-p", "mine", "back")
	oko(t, "-p", "mine", "wait", "#email")
	oko(t, "-p", "mine", "open", base+"/upload.html")
	file := filepath.Join(t.TempDir(), "note.txt")
	_ = os.WriteFile(file, []byte("hi"), 0o600)
	oko(t, "-p", "mine", "upload", "#f", file)
	must(t, oko(t, "-p", "mine", "text", "#out"), "got note.txt")

	// Cross-origin frames run in their own target: a child session through
	// the same debugger attachment.
	oko(t, "-p", "mine", "open", base+"/frames.html?"+alt)
	oko(t, "-p", "mine", "wait", "iframe")
	fs := oko(t, "-p", "mine", "snap")
	must(t, fs, `-- frame f1 "Payment"`)
	oko(t, "-p", "mine", "fill", ref(t, fs, `textbox "Card number"`), "4242")
	oko(t, "-p", "mine", "click", ref(t, fs, `button "Pay"`))
	must(t, oko(t, "-p", "mine", "snap", "--text"), "paid 4242")

	oko(t, "-p", "mine", "open", base+"/form.html")
	must(t, oko(t, "-p", "mine", "console"), "boot")
	must(t, oko(t, "-p", "mine", "net"), "missing.json")
	oko(t, "-p", "mine", "reload")
	id := strings.Fields(oko(t, "-p", "mine", "tabs"))[1]
	must(t, oko(t, "-p", "mine", "tab", id), "Form")
	must(t, oko(t, "-p", "mine", "perf"), "TTFB", "LCP")
	oko(t, "-p", "mine", "record", "start")
	oko(t, "-p", "mine", "scroll")
	gif := filepath.Join(t.TempDir(), "r.gif")
	oko(t, "-p", "mine", "record", "stop", "-o", gif)
	if fi, err := os.Stat(gif); err != nil || fi.Size() < 100 {
		t.Fatalf("recording: %v", err)
	}

	// Pages Chrome keeps from extensions are refused with a reason.
	must(t, oko(t, "!", "-p", "mine", "open", "chrome://settings"), "cannot be driven through the extension")
	ver := h.openTab("chrome://version")
	if _, err := h.js(fmt.Sprintf("oko.giveTab(%d)", ver)); err == nil || !strings.Contains(err.Error(), "cannot be handed to oko") {
		t.Fatalf("chrome:// handed over: %v", err)
	}

	// Commands that need the whole browser say so.
	must(t, oko(t, "!", "-p", "mine", "window", "maximize"), "not available on an extension profile")
	must(t, oko(t, "!", "-p", "mine", "up", "--headless"), "not available on an extension profile")

	// New tabs only with the popup's opt-in; they are marked as oko's.
	must(t, oko(t, "!", "-p", "mine", "open", "--new", base+"/article.html"), "may not open tabs")
	out = oko(t, "!", "-p", "mine", "revive")
	must(t, out, "may not open tabs")
	mustNot(t, out, "{-32000")
	h.mustJS("oko.setAllowNew(true)")
	waitFor(t, func() bool { return strings.Contains(oko(t, "ext", "status"), "may open its own tabs") })
	must(t, oko(t, "-p", "mine", "open", "--new", base+"/cards.html"), "Cards")
	must(t, oko(t, "ext", "status"), "(opened by oko)")
	oko(t, "-p", "mine", "close")

	// Refs survive giving the tab back and handing it over again.
	oko(t, "-p", "mine", "open", base+"/form.html")
	rs := oko(t, "-p", "mine", "snap")
	send := ref(t, rs, `button "Send"`)
	h.mustJS(fmt.Sprintf("oko.releaseTab(%d)", form))
	must(t, oko(t, "!", "-p", "mine", "click", send), "not handed to oko")
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", form))
	waitFor(t, func() bool { return strings.Contains(oko(t, "-p", "mine", "tabs"), "Form") })
	oko(t, "-p", "mine", "click", send)

	// perf subcommands that see the whole browser are refused.
	must(t, oko(t, "!", "-p", "mine", "perf", "trace"), "not available on an extension profile")

	// An agent session's first command takes a handed tab nobody holds;
	// then it is that session's, and others are told so.
	other2 := h.openTab(base + "/article.html")
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", other2))
	waitFor(t, func() bool { return strings.Contains(oko(t, "-p", "mine", "tabs"), "Article") })
	must(t, oko(t, "--session", "agent-1", "-p", "mine", "read"), "Article")
	must(t, oko(t, "!", "--session", "agent-2", "-p", "mine", "read"), "held by another oko session")
	h.mustJS(fmt.Sprintf("oko.releaseTab(%d)", other2))

	// Release: the tab leaves oko's reach.
	h.mustJS(fmt.Sprintf("oko.releaseTab(%d)", form))
	waitFor(t, func() bool { return !strings.Contains(oko(t, "-p", "mine", "tabs"), "Form") })
	_ = other

	// Unpaired: the extension is refused.
	oko(t, "ext", "unpair")
	if _, err := h.js("oko.connect()"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	st := h.mustJS("oko.state()")
	var state struct {
		Link string `json:"link"`
	}
	_ = json.Unmarshal([]byte(st), &state)
	if state.Link == "on" {
		t.Fatalf("unpaired extension still connected: %s", st)
	}
}
