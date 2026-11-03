package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/bridge"
	"golang.org/x/net/websocket"
)

// pairedExtension starts a throwaway Chrome with the extension, pairs it on a
// fresh port and hands over one tab (form.html).
func pairedExtension(t *testing.T) (*extHarness, *bridge.Client, int, int) {
	t.Helper()
	port := freePort(t)
	t.Setenv("OKO_EXT_PORT", strconv.Itoa(port))
	t.Setenv("OKO_PROFILE", "")
	t.Cleanup(func() { _ = exec.Command(bin, "ext", "stop").Run() })
	h := startExtChrome(t, strings.TrimSpace(oko(t, "ext", "path")))
	code := regexp.MustCompile(`pairing code: (\S+)`).FindStringSubmatch(oko(t, "ext", "pair", "--no-wait"))
	if code == nil {
		t.Fatal("pairing code missing")
	}
	h.mustJS(fmt.Sprintf("oko.setPort(%d)", port))
	h.mustJS(fmt.Sprintf("oko.pair(%q)", code[1]))
	tab := h.openTab(base + "/form.html")
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", tab))
	waitFor(t, func() bool { return strings.Contains(oko(t, "ext", "status"), "tabs handed to oko: 1") })
	c, err := bridge.NewClient(home)
	if err != nil {
		t.Fatal(err)
	}
	return h, c, tab, port
}

// TestExtensionTabBoundary: a handed-over tab's session reaches that page,
// not the profile — no cookies of other sites, no new tabs without the
// opt-in, no target list — on the bridge and in the extension itself.
func TestExtensionTabBoundary(t *testing.T) {
	h, c, tab, _ := pairedExtension(t)
	other := h.openTab(alt + "/article.html")
	_ = other
	if _, err := h.b.Call(context.Background(), "", "Storage.setCookies", map[string]interface{}{"cookies": []interface{}{
		map[string]interface{}{"name": "private_cookie", "value": "synthetic-only", "url": alt, "httpOnly": true}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u, err := c.CDPURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := rod.New().ControlURL(u).Context(ctx)
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx)
	if err != nil || len(st.Tabs) != 1 {
		t.Fatal(st, err)
	}
	att, err := proto.TargetAttachToTarget{TargetID: proto.TargetTargetID(st.Tabs[0].TargetID), Flatten: true}.Call(b)
	if err != nil {
		t.Fatal(err)
	}
	sid := string(att.SessionID)
	for _, m := range []string{"Target.getTargets", "Network.getAllCookies", "Storage.getCookies", "Network.getCookies",
		"Target.createTarget", "Target.attachToTarget", "Browser.getVersion", "Tracing.start", "Fetch.enable"} {
		raw, err := b.Call(ctx, sid, m, map[string]interface{}{"url": base + "/cards.html", "urls": []string{alt}})
		if err == nil {
			t.Errorf("%s through a tab session: allowed (%d bytes)", m, len(raw))
		}
		if strings.Contains(string(raw), "private_cookie") || strings.Contains(string(raw), alt+"/article.html") {
			t.Errorf("%s exposed another site's data", m)
		}
	}
	if !strings.Contains(oko(t, "ext", "status"), "tabs handed to oko: 1") {
		t.Error("a tab appeared without the opt-in")
	}

	// The extension enforces the same policy on its own.
	for _, tc := range []struct{ op, want string }{
		{fmt.Sprintf(`{op:'cdp', tabId:%d, method:'Network.getAllCookies'}`, tab), "not allowed"},
		{fmt.Sprintf(`{op:'cdp', tabId:%d, method:'Target.createTarget', params:{url:'about:blank'}}`, tab), "not allowed"},
		{fmt.Sprintf(`{op:'cdp', tabId:%d, method:'Page.navigate', params:{url:'file:///etc/hosts'}}`, tab), "http(s)"},
		{fmt.Sprintf(`{op:'cdp', tabId:%d, method:'Page.navigate', params:{url:'chrome://settings'}}`, tab), "cannot be handed"},
		{fmt.Sprintf(`{op:'cdp', tabId:%d, sessionId:'BOGUS', method:'Runtime.evaluate', params:{expression:'1'}}`, tab), "not a frame of this tab"},
		{fmt.Sprintf(`{op:'cdp', tabId:%d, method:'Target.setAutoAttach', params:{autoAttach:true, flatten:true, waitForDebuggerOnStart:false, filter:[{type:'page'}]}}`, tab), "only plain frame auto-attach"},
		{fmt.Sprintf(`{op:'cdp', tabId:%d, method:'Runtime.evaluate', params:{expression:'1'}}`, other), "not handed to oko"},
		{`{op:'createTab', url:'about:blank'}`, "may not open tabs"},
	} {
		_, err := h.js("oko._op(" + tc.op + ")")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("extension ran %s: %v", tc.op, err)
		}
	}
}

// TestExtensionImpersonation: something else on the bridge port after the
// bridge stopped learns nothing secret and cannot run anything in the tab.
func TestExtensionImpersonation(t *testing.T) {
	for _, reply := range []string{"welcome", "challenge"} {
		t.Run(reply, func(t *testing.T) {
			h, c, _, port := pairedExtension(t)
			if err := c.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			var ln net.Listener
			var err error
			for i := 0; i < 100; i++ {
				time.Sleep(20 * time.Millisecond)
				if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
					break
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			hello := make(chan map[string]interface{}, 1)
			after := make(chan string, 16)
			mux := http.NewServeMux()
			mux.Handle("/ext", websocket.Handler(func(ws *websocket.Conn) {
				defer func() { _ = ws.Close() }()
				_ = ws.SetDeadline(time.Now().Add(15 * time.Second))
				var m map[string]interface{}
				if websocket.JSON.Receive(ws, &m) != nil {
					return
				}
				select {
				case hello <- m:
				default:
				}
				first := map[string]interface{}{"type": "welcome"}
				if reply == "challenge" {
					first = map[string]interface{}{"type": "challenge", "nonce": strings.Repeat("a", 32), "proof": strings.Repeat("b", 64)}
				}
				_ = websocket.JSON.Send(ws, first)
				// Try to run something in the tab regardless.
				_ = websocket.JSON.Send(ws, map[string]interface{}{"id": 99, "op": "cdp", "tabId": 1, "method": "Runtime.evaluate",
					"params": map[string]interface{}{"expression": "6*7", "returnByValue": true}})
				for {
					var msg map[string]interface{}
					if websocket.JSON.Receive(ws, &msg) != nil {
						return
					}
					b, _ := json.Marshal(msg)
					after <- string(b)
				}
			}))
			srv := &http.Server{Handler: mux}
			go func() { _ = srv.Serve(ln) }()
			defer func() { _ = srv.Close() }()

			var m map[string]interface{}
			select {
			case m = <-hello:
			case <-time.After(15 * time.Second):
				t.Fatal("the extension never reconnected")
			}
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "nonce,product,type,userAgent,version" {
				t.Fatalf("hello carried more than a nonce: %v", keys)
			}
			time.Sleep(1500 * time.Millisecond)
			select {
			case s := <-after:
				t.Fatalf("the extension talked to an unproven bridge: %s", s)
			default:
			}
			var st struct {
				Link      string `json:"link"`
				LinkError string `json:"linkError"`
			}
			_ = json.Unmarshal([]byte(h.mustJS("oko.state()")), &st)
			if st.Link != "untrusted" || !strings.Contains(st.LinkError, "bridge not trusted") {
				t.Fatalf("state %+v", st)
			}
		})
	}
}

// TestExtensionReleaseReconnect: release paths detach the debugger, and a
// restarted bridge is trusted again without re-pairing.
func TestExtensionReleaseReconnect(t *testing.T) {
	h, c, tab, _ := pairedExtension(t)
	attached := func() bool {
		return h.mustJS(fmt.Sprintf(`(async()=>{const t=(await chrome.debugger.getTargets()).find(t=>t.tabId===%d);return !!t?.attached})()`, tab)) == "true"
	}
	if !attached() {
		t.Fatal("tab not attached")
	}
	h.mustJS(fmt.Sprintf("oko.releaseTab(%d)", tab))
	if attached() {
		t.Fatal("releaseTab left the debugger attached")
	}
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", tab))
	h.mustJS("oko.releaseAll()")
	if attached() {
		t.Fatal("Disconnect all left the debugger attached")
	}
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", tab))
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	must(t, oko(t, "-p", "mine", "tabs"), "Form")
	must(t, oko(t, "-p", "mine", "eval", "6*7"), "42")
	oko(t, "ext", "release")
	if attached() {
		t.Fatal("CLI release left the debugger attached")
	}
	h.mustJS(fmt.Sprintf("oko.giveTab(%d)", tab))
	oko(t, "ext", "unpair")
	if attached() {
		t.Fatal("CLI unpair left the debugger attached")
	}
}

// TestExtensionChannel: the worker's frame channel refuses tampered,
// replayed, reordered, reflected and forged frames (protocol.js, run in the
// throwaway Chrome's extension worker).
func TestExtensionChannel(t *testing.T) {
	port := freePort(t)
	t.Setenv("OKO_EXT_PORT", strconv.Itoa(port))
	h := startExtChrome(t, strings.TrimSpace(oko(t, "ext", "path")))
	out := h.mustJS(`(async () => {
		const key = new Uint8Array(32).fill(7), ne = 'a'.repeat(32), nb = 'b'.repeat(32);
		const pair = async () => [await oko._channel(key, ne, nb, 'bridge'), await oko._channel(key, ne, nb, 'ext')];
		const refused = async (ex, f) => { try { await ex.open(f); return false; } catch { return true; } };
		const res = {};
		{ const [br, ex] = await pair();
		  res.genuine = (await ex.open(await br.seal('{"id":1}'))) === '{"id":1}' && (await ex.open(await br.seal('{"id":2}'))) === '{"id":2}'; }
		{ const [br, ex] = await pair(); const f = await br.seal('{"method":"Page.reload"}');
		  res.tampered = await refused(ex, f.replace('Page.reload', 'Runtime.evaluate')); }
		{ const [br, ex] = await pair(); const f = await br.seal('{"id":1}'); await ex.open(f);
		  res.replayed = await refused(ex, f); }
		{ const [br, ex] = await pair(); await br.seal('{"id":1}');
		  res.reordered = await refused(ex, await br.seal('{"id":2}')); }
		{ const [br, ex] = await pair();
		  res.reflected = await refused(ex, await ex.seal('{"id":1}')); }
		{ const [br, ex] = await pair();
		  res.unsealed = await refused(ex, '{"id":99,"op":"releaseAll"}'); }
		{ const [br, ex] = await pair();
		  res.forged = await refused(ex, '1 ' + '0'.repeat(64) + ' {"id":99,"op":"releaseAll"}'); }
		{ const [br, ex] = await pair(); await refused(ex, 'junk');
		  res.closedAfterBad = await refused(ex, await br.seal('{"id":1}')); }
		return res;
	})()`)
	var res map[string]bool
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(out, err)
	}
	for _, k := range []string{"genuine", "tampered", "replayed", "reordered", "reflected", "unsealed", "forged", "closedAfterBad"} {
		if !res[k] {
			t.Errorf("%s: %s", k, out)
		}
	}
}

// TestExtensionRelay: a listener on the port the extension dials forwards a
// genuine handshake to the paired bridge and then injects a command of its
// own. The extension drops the session and runs nothing.
func TestExtensionRelay(t *testing.T) {
	for _, inject := range []string{"unsealed", "forged"} {
		t.Run(inject, func(t *testing.T) {
			h, c, tab, port := pairedExtension(t)
			title := func() string {
				return h.mustJS(fmt.Sprintf(`(async () => (await chrome.tabs.get(%d)).title)()`, tab))
			}
			before := title()
			op := fmt.Sprintf(`{"id":99,"op":"cdp","tabId":%d,"method":"Runtime.evaluate","params":{"expression":"document.title='relayed'"}}`, tab)
			frame := op
			if inject == "forged" {
				frame = "1 " + strings.Repeat("0", 64) + " " + op
			}
			relayed := make(chan struct{}, 1)
			fromExt := make(chan string, 64)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.Handle("/ext", websocket.Handler(func(ext *websocket.Conn) {
				defer func() { _ = ext.Close() }()
				cfg, _ := websocket.NewConfig(fmt.Sprintf("ws://127.0.0.1:%d/ext", port), ext.Request().Header.Get("Origin"))
				var br *websocket.Conn
				for i := 0; i < 100 && br == nil; i++ {
					var derr error
					if br, derr = websocket.DialConfig(cfg); derr != nil {
						br = nil
						time.Sleep(100 * time.Millisecond)
					}
				}
				if br == nil {
					return
				}
				defer func() { _ = br.Close() }()
				go func() {
					for {
						var m string
						if websocket.Message.Receive(ext, &m) != nil {
							_ = br.Close()
							return
						}
						fromExt <- m
						_ = websocket.Message.Send(br, m)
					}
				}()
				for {
					var m string
					if websocket.Message.Receive(br, &m) != nil {
						return
					}
					if websocket.Message.Send(ext, m) != nil {
						return
					}
					if strings.Contains(m, `"welcome"`) {
						_ = websocket.Message.Send(ext, frame)
						relayed <- struct{}{}
					}
				}
			}))
			srv := &http.Server{Handler: mux}
			go func() { _ = srv.Serve(ln) }()
			defer func() { _ = srv.Close() }()

			// Point the extension at the relay and bring the bridge back
			// behind it.
			h.mustJS(fmt.Sprintf("oko.setPort(%d)", ln.Addr().(*net.TCPAddr).Port))
			if err := c.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return !c.Running(context.Background()) })
			go func() { _ = exec.Command(bin, "-p", "mine", "tabs").Run() }()
			select {
			case <-relayed:
			case <-time.After(20 * time.Second):
				t.Fatal("the handshake never went through the relay")
			}
			var st struct {
				Link      string `json:"link"`
				LinkError string `json:"linkError"`
			}
			waitFor(t, func() bool {
				_ = json.Unmarshal([]byte(h.mustJS("oko.state()")), &st)
				return st.Link == "untrusted"
			})
			if !strings.Contains(st.LinkError, "failed authentication") {
				t.Fatalf("state %+v", st)
			}
			// The toolbar shows the dropped session: red "!" and its title.
			if got := h.mustJS(`chrome.action.getBadgeText({}).then(async (b) => b + "|" + await chrome.action.getTitle({}))`); got != `"!|oko: link closed"` {
				t.Fatalf("toolbar after a failed frame: %s", got)
			}
			time.Sleep(500 * time.Millisecond)
			if got := title(); got != before || got == "relayed" {
				t.Fatalf("the injected command ran: title %q", got)
			}
			for {
				select {
				case m := <-fromExt:
					if strings.Contains(m, `"id":99`) {
						t.Fatalf("the extension answered the injected command: %s", m)
					}
					continue
				default:
				}
				break
			}
			// A closed link lets go of every tab: the debugger is off the
			// handed tab and oko holds nothing until the user gives one again.
			attached := func() bool {
				return h.mustJS(fmt.Sprintf(`(async()=>{const t=(await chrome.debugger.getTargets()).find(t=>t.tabId===%d);return !!t?.attached})()`, tab)) == "true"
			}
			waitFor(t, func() bool { return !attached() })
			var after struct {
				Tabs     []json.RawMessage `json:"tabs"`
				Dropped  int               `json:"dropped"`
				Released map[string]string `json:"released"`
			}
			_ = json.Unmarshal([]byte(h.mustJS("oko.state()")), &after)
			if len(after.Tabs) != 0 || after.Dropped != 1 || after.Released[strconv.Itoa(tab)] != "the link failed a security check" {
				t.Fatalf("tabs after a failed frame: %+v", after)
			}
		})
	}
}
