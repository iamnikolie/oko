package bridge

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

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

func (b *testBridge) client() *Client { return &Client{Port: b.port, secret: b.secret} }

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

var errBridgeProof = fmt.Errorf("bridge proof did not check out")

// extPair plays the extension's side of pairing (extension/protocol.js).
func extPair(t *testing.T, b *testBridge, origin, code string) ([]byte, *fakeExt, map[string]string, error) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubExt := hex.EncodeToString(priv.PublicKey().Bytes())
	nonce, _ := randomHex(16)
	e, reply := dialExt(t, b, origin, map[string]string{"type": "pair", "pub": pubExt, "nonce": nonce, "version": "test", "product": "Chrome/1"})
	if reply["type"] != "pair-challenge" {
		return nil, e, reply, fmt.Errorf("pair: %v", reply)
	}
	tr := pairTranscript(origin, pubExt, reply["pub"], nonce, reply["nonce"])
	ck := codeKey(code, reply["nonce"])
	if !hmacOK(ck, "bridge|"+tr, reply["proof"]) {
		return nil, e, reply, errBridgeProof
	}
	pb, _ := hex.DecodeString(reply["pub"])
	peer, err := ecdh.P256().NewPublicKey(pb)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := priv.ECDH(peer)
	key := hmacSum(shared, "oko-key|"+tr)
	final := e.exchange(t, map[string]string{"type": "pair-confirm", "proof": hmacHex(ck, "ext|"+tr)})
	if final["type"] != "paired" {
		return nil, e, final, fmt.Errorf("confirm: %v", final)
	}
	return key, e, final, nil
}

// extHello plays a returning extension: it checks the bridge's proof first.
func extHello(t *testing.T, b *testBridge, origin string, key []byte) (*fakeExt, map[string]string, error) {
	t.Helper()
	nonce, _ := randomHex(16)
	e, reply := dialExt(t, b, origin, map[string]string{"type": "hello", "nonce": nonce, "version": "test", "product": "Chrome/1"})
	if reply["type"] != "challenge" {
		return e, reply, fmt.Errorf("hello: %v", reply)
	}
	if !hmacOK(key, helloBridgeMsg(origin, nonce, reply["nonce"]), reply["proof"]) {
		return e, reply, errBridgeProof
	}
	final := e.exchange(t, map[string]string{"type": "auth", "proof": hmacHex(key, helloExtMsg(origin, nonce, reply["nonce"]))})
	if final["type"] != "welcome" {
		return e, final, fmt.Errorf("auth: %v", final)
	}
	return e, final, nil
}

func (e *fakeExt) exchange(t *testing.T, msg map[string]string) map[string]string {
	t.Helper()
	if err := websocket.JSON.Send(e.ws, msg); err != nil {
		t.Fatal(err)
	}
	var reply map[string]string
	_ = e.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := websocket.JSON.Receive(e.ws, &reply); err != nil {
		return map[string]string{"type": "closed"}
	}
	_ = e.ws.SetReadDeadline(time.Time{})
	return reply
}

// paired returns a connected, paired fake extension.
func paired(t *testing.T, b *testBridge) *fakeExt {
	t.Helper()
	code, err := b.srv.pair.NewCode()
	if err != nil {
		t.Fatal(err)
	}
	_, e, _, err := extPair(t, b, testOrigin, code)
	if err != nil {
		t.Fatal(err)
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
	u, err := b.client().CDPURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cdp.StartWithURL(context.Background(), u, nil)
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
	c := b.client()
	if err := c.auth(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	ok := map[string]string{"X-Oko-Ticket": c.ticket}
	if code := post("/control/status", ok); code != http.StatusOK {
		t.Fatalf("status with ticket: %d", code)
	}
	if code := post("/control/status", nil); code != http.StatusUnauthorized {
		t.Fatalf("no ticket: %d", code)
	}
	if code := post("/control/status", map[string]string{"X-Oko-Ticket": strings.Repeat("0", 64)}); code != http.StatusUnauthorized {
		t.Fatalf("wrong ticket: %d", code)
	}
	if code := post("/control/status", map[string]string{"X-Oko-Secret": b.secret}); code != http.StatusUnauthorized {
		t.Fatalf("the long-term secret works as a bearer: %d", code)
	}
	if code := post("/control/status", map[string]string{"X-Oko-Ticket": c.ticket, "Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("browser origin on the client API: %d", code)
	}
	if code := post("/control/status", map[string]string{"X-Oko-Ticket": c.ticket, "Host": "evil.example:" + fmt.Sprint(b.port)}); code != http.StatusForbidden {
		t.Fatalf("rebound host: %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+b.url("/control/status"), http.NoBody)
	req.Header.Set("X-Oko-Ticket", c.ticket)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on control: %v %v", resp, err)
	}
	// A CLI that cannot prove the secret gets no ticket.
	bad := &Client{Port: b.port, secret: strings.Repeat("f", 64)}
	if err := bad.auth(context.Background()); err == nil {
		t.Fatal("ticket for a client without the secret")
	}

	// A web page cannot open the CDP socket even with a ticket: browsers
	// always send an Origin on websockets.
	cfg, _ := websocket.NewConfig("ws://"+b.url("/cdp/"+c.ticket), "https://evil.example")
	if _, err := websocket.DialConfig(cfg); err == nil {
		t.Fatal("CDP socket opened from a web origin")
	}
	for _, path := range []string{"/cdp/wrong", "/cdp/" + b.secret} {
		if _, err := cdp.StartWithURL(context.Background(), "ws://"+b.url(path), nil); err == nil {
			t.Fatalf("CDP socket opened with %s", path)
		}
	}
}

// TestCLIRefusesImpostor: a listener on the port that does not hold the
// client secret is refused, and the secret never reaches it.
func TestCLIRefusesImpostor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.URL.String()+" "+fmt.Sprint(r.Header)+" "+string(body))
		mu.Unlock()
		// It cannot compute the proof; it echoes something plausible.
		_ = json.NewEncoder(w).Encode(map[string]string{"nonce": strings.Repeat("a", 32), "proof": strings.Repeat("b", 64), "ticket": "t"})
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	secret := strings.Repeat("5", 64)
	c := &Client{Port: ln.Addr().(*net.TCPAddr).Port, secret: secret}
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrNotOurBridge) {
		t.Fatalf("impostor accepted: %v", err)
	}
	if _, err := c.CDPURL(context.Background()); !errors.Is(err, ErrNotOurBridge) {
		t.Fatalf("CDP URL from an impostor: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, secret) || strings.Contains(s, "auth/finish") || strings.Contains(s, "/control/status") {
			t.Fatalf("impostor got more than a nonce: %s", s)
		}
	}
}

func TestExtensionAuth(t *testing.T) {
	b := newTestBridge(t)
	if _, r, err := extHello(t, b, testOrigin, make([]byte, 32)); err == nil || r["type"] != "refused" || !strings.Contains(r["reason"], "not paired") {
		t.Fatalf("unpaired hello: %v %v", r, err)
	}
	code, _ := b.srv.pair.NewCode()
	if _, _, r, err := extPair(t, b, "https://evil.example", code); err == nil || r["type"] != "refused" {
		t.Fatalf("pairing from a web origin: %v %v", r, err)
	}
	// A wrong code: the extension sees the bridge's proof fail and stops; a
	// forged confirmation is refused by the bridge.
	if _, _, _, err := extPair(t, b, testOrigin, "WRON-GCOD"); err != errBridgeProof {
		t.Fatalf("wrong code: %v", err)
	}
	nonce, _ := randomHex(16)
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	e, r := dialExt(t, b, testOrigin, map[string]string{"type": "pair", "pub": hex.EncodeToString(priv.PublicKey().Bytes()), "nonce": nonce})
	if r["type"] != "pair-challenge" {
		t.Fatalf("challenge: %v", r)
	}
	if r := e.exchange(t, map[string]string{"type": "pair-confirm", "proof": strings.Repeat("0", 64)}); r["type"] != "refused" {
		t.Fatalf("forged confirmation: %v", r)
	}
	key, _, _, err := extPair(t, b, testOrigin, code)
	if err != nil {
		t.Fatal(err)
	}
	if saved, _ := b.srv.pair.key(testOrigin); !bytes.Equal(saved, key) {
		t.Fatal("the two sides derived different keys")
	}
	if _, _, _, err := extPair(t, b, testOrigin, code); err == nil {
		t.Fatal("pairing code worked twice")
	}
	if _, r, err := extHello(t, b, testOrigin, key); err != nil {
		t.Fatalf("paired hello: %v %v", r, err)
	}
	// A second link (worker restart) replaces the first without harm.
	if _, r, err := extHello(t, b, testOrigin, key); err != nil || !b.srv.Status().Connected {
		t.Fatalf("second hello: %v %v", r, err)
	}
	if _, r, err := extHello(t, b, "chrome-extension://bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", key); err == nil || r["type"] != "refused" {
		t.Fatalf("key from another extension: %v %v", r, err)
	}
	wrong := append([]byte{}, key...)
	wrong[0] ^= 0xff
	if _, _, err := extHello(t, b, testOrigin, wrong); err != errBridgeProof {
		t.Fatalf("an extension with another key trusted the bridge: %v", err)
	}
	// An extension that cannot prove the key is refused even after the
	// bridge proved itself.
	nonce, _ = randomHex(16)
	e, r = dialExt(t, b, testOrigin, map[string]string{"type": "hello", "nonce": nonce})
	if r["type"] != "challenge" {
		t.Fatalf("challenge: %v", r)
	}
	if r := e.exchange(t, map[string]string{"type": "auth", "proof": strings.Repeat("0", 64)}); r["type"] != "refused" {
		t.Fatalf("unproven extension admitted: %v", r)
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
	if _, err := c.call(t, sid, "Page.reload", nil); err != nil {
		t.Fatalf("allowed method: %v", err)
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

	// Only page-scoped methods pass: cookies, storage, targets and the browser
	// stay out of reach of a tab session.
	for _, m := range []string{"Network.getAllCookies", "Network.getCookies", "Storage.getCookies", "Target.createTarget",
		"Target.getTargets", "Target.attachToTarget", "Target.setAutoAttach", "Browser.getVersion", "Tracing.start", "Fetch.enable"} {
		if _, err := c.call(t, sid, m, map[string]string{"url": "about:blank"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s through a tab session: %v", m, err)
		}
		if len(e.seen(m)) != 0 {
			t.Errorf("%s reached the extension", m)
		}
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
	if _, err := c.call(t, "", "Target.detachFromTarget", map[string]string{"sessionId": sid}); err != nil {
		t.Fatal(err)
	}
	if rm := e.seen("Page.removeScriptToEvaluateOnNewDocument"); len(rm) != 1 || !strings.Contains(string(rm[0].Params), `"7"`) {
		t.Fatalf("script not removed: %+v", rm)
	}
	if len(e.seen("Runtime.disable")) == 0 || len(e.seen("Page.disable")) == 0 {
		t.Fatal("domains left on after the last session")
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
