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
