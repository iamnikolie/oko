package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"golang.org/x/net/websocket"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type testBridge struct {
	t      *testing.T
	srv    *Server
	port   int
	secret string
}

func newTestBridge(t *testing.T) *testBridge {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := NewServer(port, "s3cret-s3cret-s3cret-s3cret-s3cret", LoadPairing(filepath.Join(t.TempDir(), "paired.json")))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return &testBridge{t: t, srv: srv, port: port, secret: srv.secret}
}

func (b *testBridge) url(path string) string { return fmt.Sprintf("127.0.0.1:%d%s", b.port, path) }

// fakeExt plays the extension: it answers ops from a script and records them.
type fakeExt struct {
	ws  *websocket.Conn
	mu  sync.Mutex
	ops []extReq
	// answer returns the result for an op (nil error → result).
	answer func(r extReq) (interface{}, string)
}

func dialExt(t *testing.T, b *testBridge, origin string, hello map[string]string) (*fakeExt, map[string]string) {
	t.Helper()
	cfg, err := websocket.NewConfig("ws://"+b.url("/ext"), origin)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("dial /ext: %v", err)
	}
	if err := websocket.JSON.Send(ws, hello); err != nil {
		t.Fatal(err)
	}
	var reply map[string]string
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := websocket.JSON.Receive(ws, &reply); err != nil {
		t.Fatalf("no reply to %v: %v", hello, err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	return &fakeExt{ws: ws}, reply
}

func (e *fakeExt) serve() {
	for {
		var r extReq
		if err := websocket.JSON.Receive(e.ws, &r); err != nil {
			return
		}
		e.mu.Lock()
		e.ops = append(e.ops, r)
		ans := e.answer
		e.mu.Unlock()
		var res interface{} = map[string]interface{}{}
		errMsg := ""
		if ans != nil {
			res, errMsg = ans(r)
		}
		if errMsg != "" {
			_ = websocket.JSON.Send(e.ws, map[string]interface{}{"id": r.ID, "error": map[string]interface{}{"code": -32000, "message": errMsg}})
		} else {
			_ = websocket.JSON.Send(e.ws, map[string]interface{}{"id": r.ID, "result": res})
		}
	}
}

func (e *fakeExt) seen(method string) []extReq {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []extReq
	for _, r := range e.ops {
		if r.Method == method || r.Op == method {
			out = append(out, r)
		}
	}
	return out
}

func (e *fakeExt) tabs(t *testing.T, allowNew bool, tabs ...Tab) {
	t.Helper()
	if err := websocket.JSON.Send(e.ws, map[string]interface{}{"ev": "tabs", "tabs": tabs, "allowNew": allowNew}); err != nil {
		t.Fatal(err)
	}
}

// paired returns a connected, paired fake extension.
func paired(t *testing.T, b *testBridge) *fakeExt {
	t.Helper()
	code, err := b.srv.pair.NewCode()
	if err != nil {
		t.Fatal(err)
	}
	e, reply := dialExt(t, b, testOrigin, map[string]string{"type": "pair", "code": code, "version": "test", "product": "Chrome/1"})
	if reply["type"] != "paired" || reply["token"] == "" {
		t.Fatalf("pair reply %v", reply)
	}
	go e.serve()
	waitUntil(t, func() bool { return b.srv.Status().Connected })
	return e
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

type cdpConn struct {
	c      *cdp.Client
	events chan *cdp.Event
}

func dialCDP(t *testing.T, b *testBridge) *cdpConn {
	t.Helper()
	c, err := cdp.StartWithURL(context.Background(), "ws://"+b.url("/cdp/"+b.secret), nil)
	if err != nil {
		t.Fatalf("dial /cdp: %v", err)
	}
	cc := &cdpConn{c: c, events: make(chan *cdp.Event, 100)}
	go func() {
		for e := range c.Event() {
			cc.events <- e
		}
	}()
	return cc
}

// next waits for the next event with this method.
func (c *cdpConn) next(t *testing.T, method string) *cdp.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-c.events:
			if ev.Method == method {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event", method)
			return nil
		}
	}
}

func (c *cdpConn) call(t *testing.T, session, method string, params interface{}) (map[string]interface{}, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := c.c.Call(ctx, session, method, params)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func TestGuard(t *testing.T) {
	b := newTestBridge(t)
	post := func(path string, hdr map[string]string) int {
		req, _ := http.NewRequest(http.MethodPost, "http://"+b.url(path), http.NoBody)
		for k, v := range hdr {
			if k == "Host" {
				req.Host = v
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	ok := map[string]string{"X-Oko-Secret": b.secret}
	if c := post("/control/status", ok); c != http.StatusOK {
		t.Fatalf("status with secret: %d", c)
	}
	if c := post("/control/status", nil); c != http.StatusUnauthorized {
		t.Fatalf("no secret: %d", c)
	}
	if c := post("/control/status", map[string]string{"X-Oko-Secret": "nope"}); c != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", c)
	}
	if c := post("/control/status", map[string]string{"X-Oko-Secret": b.secret, "Origin": "https://evil.example"}); c != http.StatusForbidden {
		t.Fatalf("browser origin on the client API: %d", c)
	}
	if c := post("/control/status", map[string]string{"X-Oko-Secret": b.secret, "Host": "evil.example:" + fmt.Sprint(b.port)}); c != http.StatusForbidden {
		t.Fatalf("rebound host: %d", c)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+b.url("/control/status"), http.NoBody)
	req.Header.Set("X-Oko-Secret", b.secret)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on control: %v %v", resp, err)
	}

	// A web page cannot open the CDP socket even with the secret: browsers
	// always send an Origin on websockets.
	cfg, _ := websocket.NewConfig("ws://"+b.url("/cdp/"+b.secret), "https://evil.example")
	if _, err := websocket.DialConfig(cfg); err == nil {
		t.Fatal("CDP socket opened from a web origin")
	}
	if _, err := cdp.StartWithURL(context.Background(), "ws://"+b.url("/cdp/wrong"), nil); err == nil {
		t.Fatal("CDP socket opened with a wrong secret")
	}
}

func TestExtensionAuth(t *testing.T) {
	b := newTestBridge(t)
	_, r := dialExt(t, b, testOrigin, map[string]string{"type": "hello", "token": "whatever"})
	if r["type"] != "refused" || !strings.Contains(r["reason"], "not paired") {
		t.Fatalf("unpaired hello: %v", r)
	}
	code, _ := b.srv.pair.NewCode()
	_, r = dialExt(t, b, "https://evil.example", map[string]string{"type": "pair", "code": code})
	if r["type"] != "refused" {
		t.Fatalf("pairing from a web origin: %v", r)
	}
	_, r = dialExt(t, b, testOrigin, map[string]string{"type": "pair", "code": code})
	token := r["token"]
	if r["type"] != "paired" || token == "" {
		t.Fatalf("pair: %v", r)
	}
	_, r = dialExt(t, b, testOrigin, map[string]string{"type": "hello", "token": token})
	if r["type"] != "welcome" {
		t.Fatalf("paired hello: %v", r)
	}
	// A second link (worker restart) replaces the first without harm.
	_, r = dialExt(t, b, testOrigin, map[string]string{"type": "hello", "token": token})
	if r["type"] != "welcome" || !b.srv.Status().Connected {
		t.Fatalf("second hello: %v", r)
	}
	_, r = dialExt(t, b, "chrome-extension://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", map[string]string{"type": "hello", "token": token})
	if r["type"] != "refused" {
		t.Fatalf("token from another extension: %v", r)
	}
	_, r = dialExt(t, b, testOrigin, map[string]string{"type": "hello", "token": strings.Repeat("0", 64)})
	if r["type"] != "refused" {
		t.Fatalf("unknown token: %v", r)
	}
}

func TestRelay(t *testing.T) {
	b := newTestBridge(t)
	e := paired(t, b)
	e.mu.Lock()
	e.answer = func(r extReq) (interface{}, string) {
		switch r.Method {
		case "Runtime.evaluate":
			return map[string]interface{}{"result": map[string]interface{}{"type": "number", "value": 2}}, ""
		case "Page.addScriptToEvaluateOnNewDocument":
			return map[string]string{"identifier": "7"}, ""
		case "Bad.method":
			return nil, "'Bad.method' wasn't found"
		}
		return map[string]interface{}{}, ""
	}
	e.mu.Unlock()

	c := dialCDP(t, b)
	if r, _ := c.call(t, "", "Target.getTargets", nil); len(r["targetInfos"].([]interface{})) != 0 {
		t.Fatalf("targets before any hand-over: %v", r)
	}
	if _, err := c.call(t, "", "Target.attachToTarget", map[string]interface{}{"targetId": "T-OTHER", "flatten": true}); err == nil ||
		!strings.Contains(err.Error(), "not handed to oko") {
		t.Fatalf("attach to a tab not handed over: %v", err)
	}
	if _, err := c.call(t, "", "Target.createTarget", map[string]string{"url": "about:blank"}); err == nil || !strings.Contains(err.Error(), "no tab is handed") {
		t.Fatalf("create without tabs: %v", err)
	}

	e.tabs(t, false, Tab{TabID: 7, TargetID: "T7", URL: "https://a.example/", Title: "A"})
	waitUntil(t, func() bool { return len(b.srv.Status().Tabs) == 1 })
	r, _ := c.call(t, "", "Target.getTargets", nil)
	ti := r["targetInfos"].([]interface{})[0].(map[string]interface{})
	if ti["targetId"] != "T7" || ti["type"] != "page" || ti["url"] != "https://a.example/" {
		t.Fatalf("target info %v", ti)
	}
	r, err := c.call(t, "", "Target.attachToTarget", map[string]interface{}{"targetId": "T7", "flatten": true})
	if err != nil {
		t.Fatal(err)
	}
	sid := r["sessionId"].(string)

	// Per-tab commands go to the extension with the tab id.
	r, err = c.call(t, sid, "Runtime.evaluate", map[string]string{"expression": "1+1"})
	if err != nil || r["result"].(map[string]interface{})["value"].(float64) != 2 {
		t.Fatalf("evaluate: %v %v", r, err)
	}
	if ops := e.seen("Runtime.evaluate"); len(ops) != 1 || ops[0].TabID != 7 || ops[0].Op != "cdp" {
		t.Fatalf("relayed ops %+v", ops)
	}
	if _, err := c.call(t, sid, "Bad.method", nil); err == nil || !strings.Contains(err.Error(), "wasn't found") {
		t.Fatalf("extension error not relayed: %v", err)
	}
	if _, err := c.call(t, "NOPE", "Runtime.evaluate", nil); err == nil {
		t.Fatal("unknown session accepted")
	}

	// Restricted pages are refused before reaching Chrome.
	if _, err := c.call(t, sid, "Page.navigate", map[string]string{"url": "chrome://settings"}); err == nil {
		t.Fatal("navigation to chrome:// relayed")
	}
	if len(e.seen("Page.navigate")) != 0 {
		t.Fatal("restricted navigation reached the extension")
	}

	// Events of the tab reach the session.
	_ = websocket.JSON.Send(e.ws, map[string]interface{}{"ev": "cdp", "tabId": 7, "method": "Page.loadEventFired", "params": map[string]float64{"timestamp": 1}})
	if ev := c.next(t, "Page.loadEventFired"); ev.SessionID != sid {
		t.Fatalf("event %+v", ev)
	}

	// Browser-level CDP is refused with a reason.
	if _, err := c.call(t, "", "Browser.setWindowBounds", nil); err == nil || !strings.Contains(err.Error(), "not available on an extension profile") {
		t.Fatalf("browser-level call: %v", err)
	}

	// New tabs only with the opt-in.
	if _, err := c.call(t, "", "Target.createTarget", map[string]string{"url": "about:blank"}); err == nil || !strings.Contains(err.Error(), "may not open tabs") {
		t.Fatalf("create without opt-in: %v", err)
	}
	e.mu.Lock()
	prev := e.answer
	e.answer = func(r extReq) (interface{}, string) {
		if r.Op == "createTab" {
			return Tab{TabID: 9, TargetID: "T9", URL: "about:blank", Own: true}, ""
		}
		return prev(r)
	}
	e.mu.Unlock()
	e.tabs(t, true, Tab{TabID: 7, TargetID: "T7", URL: "https://a.example/", Title: "A"})
	waitUntil(t, func() bool { return b.srv.Status().AllowNew })
	if r, err := c.call(t, "", "Target.createTarget", map[string]interface{}{"url": "about:blank", "background": true}); err != nil || r["targetId"] != "T9" {
		t.Fatalf("create with opt-in: %v %v", r, err)
	}

	// What a session leaves on the shared attachment is undone when it ends.
	if _, err := c.call(t, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]string{"source": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.call(t, sid, "Fetch.enable", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.call(t, "", "Target.detachFromTarget", map[string]string{"sessionId": sid}); err != nil {
		t.Fatal(err)
	}
	if rm := e.seen("Page.removeScriptToEvaluateOnNewDocument"); len(rm) != 1 || !strings.Contains(string(rm[0].Params), `"7"`) {
		t.Fatalf("script not removed: %+v", rm)
	}
	if len(e.seen("Fetch.disable")) == 0 || len(e.seen("Runtime.disable")) == 0 {
		t.Fatal("interception / domains left on after the last session")
	}

	// A tab the user releases disappears for oko.
	r, _ = c.call(t, "", "Target.attachToTarget", map[string]interface{}{"targetId": "T7", "flatten": true})
	sid = r["sessionId"].(string)
	e.tabs(t, true)
	waitUntil(t, func() bool { return len(b.srv.Status().Tabs) == 0 })
	if _, err := c.call(t, sid, "Runtime.evaluate", nil); err == nil {
		t.Fatal("released tab still reachable")
	}
}

func TestFrameTarget(t *testing.T) {
	b := newTestBridge(t)
	e := paired(t, b)
	e.mu.Lock()
	e.answer = func(r extReq) (interface{}, string) {
		if r.Method == "Target.setAutoAttach" {
			// Chrome reports the out-of-process frame to the tab session.
			go func() {
				_ = websocket.JSON.Send(e.ws, map[string]interface{}{"ev": "cdp", "tabId": 7, "method": "Target.attachedToTarget",
					"params": map[string]interface{}{"sessionId": "CHILD1", "targetInfo": map[string]string{"targetId": "FRAME1", "type": "iframe"}}})
			}()
		}
		return map[string]interface{}{}, ""
	}
	e.mu.Unlock()
	e.tabs(t, false, Tab{TabID: 7, TargetID: "T7", URL: "https://a.example/"})
	waitUntil(t, func() bool { return len(b.srv.Status().Tabs) == 1 })
	c := dialCDP(t, b)
	r, err := c.call(t, "", "Target.attachToTarget", map[string]interface{}{"targetId": "FRAME1", "flatten": true})
	if err != nil {
		t.Fatal(err)
	}
	sid := r["sessionId"].(string)
	if _, err := c.call(t, sid, "Runtime.evaluate", map[string]string{"expression": "1"}); err != nil {
		t.Fatal(err)
	}
	ops := e.seen("Runtime.evaluate")
	if len(ops) != 1 || ops[0].TabID != 7 || ops[0].SessionID != "CHILD1" {
		t.Fatalf("frame command not routed to the child session: %+v", ops)
	}
	_ = websocket.JSON.Send(e.ws, map[string]interface{}{"ev": "cdp", "tabId": 7, "sessionId": "CHILD1", "method": "Runtime.consoleAPICalled", "params": map[string]string{}})
	if ev := c.next(t, "Runtime.consoleAPICalled"); ev.SessionID != sid {
		t.Fatalf("frame event under %q, want %q", ev.SessionID, sid)
	}
}
