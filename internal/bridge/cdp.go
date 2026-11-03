package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// cdpClient is one oko command (or a helper process such as the recorder)
// talking CDP to the bridge as if it were Chrome.
type cdpClient struct {
	ws     *websocket.Conn
	sendMu sync.Mutex
	ctx    context.Context
}

// vsession is a CDP session a client opened on a tab with
// Target.attachToTarget. The extension holds one debugger attachment per tab;
// every client session on that tab shares it, so the bridge undoes what a
// session left behind when it ends (scripts, bindings, interception,
// emulation), the way Chrome does when a real session detaches.
type vsession struct {
	id       string
	tabID    int
	targetID string
	// child is the real session id of an out-of-process frame (or worker)
	// target of the tab, reached through the tab's attachment; "" for the
	// tab itself.
	child  string
	client *cdpClient
	undo   map[string]undoCall
}

// frameRef locates a child target (out-of-process frame) of a handed-over tab.
type frameRef struct {
	tabID   int
	session string
}

// frameWait bounds how long attaching to a frame waits for Chrome to report it.
const frameWait = 3 * time.Second

type undoCall struct {
	method string
	params interface{}
}

type cdpReq struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpResp struct {
	ID        int64           `json:"id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpEvent struct {
	Method    string      `json:"method"`
	Params    interface{} `json:"params"`
	SessionID string      `json:"sessionId,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return e.Message }

func (c *cdpClient) send(v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_ = websocket.Message.Send(c.ws, string(b))
}

func (s *Server) cdpHandler() websocket.Server {
	return newWS(func(ws *websocket.Conn) {
		ctx, cancel := context.WithCancel(context.Background())
		c := &cdpClient{ws: ws, ctx: ctx}
		s.mu.Lock()
		s.clients[c] = true
		s.mu.Unlock()
		defer func() {
			cancel()
			s.dropClient(c)
			_ = ws.Close()
		}()
		for {
			var raw string
			if err := websocket.Message.Receive(ws, &raw); err != nil {
				return
			}
			var req cdpReq
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				continue
			}
			go s.handle(c, req)
		}
	})
}

func (s *Server) handle(c *cdpClient, req cdpReq) {
	var res json.RawMessage
	var err error
	if req.SessionID == "" {
		res, err = s.browserCall(c, req)
	} else {
		res, err = s.sessionCall(c, req)
	}
	resp := cdpResp{ID: req.ID, SessionID: req.SessionID}
	if err != nil {
		ce, ok := err.(*cdpError)
		if !ok {
			ce = &cdpError{Code: -32000, Message: err.Error()}
		}
		resp.Error = ce
	} else {
		if len(res) == 0 {
			res = json.RawMessage("{}")
		}
		resp.Result = res
	}
	c.send(resp)
}

func notAvailable(method string) error {
	return &cdpError{Code: -32601, Message: method + ": not available on an extension profile " +
		"(the extension drives single tabs; it has no browser-level access)"}
}

var errNoTab = &cdpError{Code: -32000, Message: "no tab is handed to oko: click the oko icon on a tab and press " +
	"\"Give this tab to oko\" (or allow oko to open its own tabs in the popup)"}

// browserCall answers the browser-level part of CDP from the handed-over tabs.
func (s *Server) browserCall(c *cdpClient, req cdpReq) (json.RawMessage, error) {
	var p struct {
		TargetID   string `json:"targetId"`
		SessionID  string `json:"sessionId"`
		URL        string `json:"url"`
		NewWindow  bool   `json:"newWindow"`
		Background bool   `json:"background"`
	}
	_ = json.Unmarshal(req.Params, &p)
	ctx, cancel := context.WithTimeout(c.ctx, extCallTimeout)
	defer cancel()
	switch req.Method {
	case "Target.setDiscoverTargets", "Target.setAutoAttach", "Target.setRemoteLocations":
		return nil, nil
	case "Browser.getVersion":
		s.mu.Lock()
		defer s.mu.Unlock()
		return marshal(map[string]string{"protocolVersion": "1.3", "product": s.product + " (oko extension)",
			"revision": "", "userAgent": s.userAgent, "jsVersion": ""})
	case "Target.getTargets":
		s.mu.Lock()
		defer s.mu.Unlock()
		infos := []interface{}{}
		for _, t := range s.tabListLocked() {
			infos = append(infos, targetInfo(t))
		}
		return marshal(map[string]interface{}{"targetInfos": infos})
	case "Target.getTargetInfo":
		t := s.tabByTarget(p.TargetID)
		if t == nil {
			return nil, &cdpError{Code: -32602, Message: "No target with given id found"}
		}
		return marshal(map[string]interface{}{"targetInfo": targetInfo(*t)})
	case "Target.attachToTarget":
		vs := &vsession{targetID: p.TargetID, client: c, undo: map[string]undoCall{}}
		if t := s.tabByTarget(p.TargetID); t != nil {
			vs.tabID = t.TabID
		} else if f, ok := s.findFrame(ctx, p.TargetID); ok {
			vs.tabID, vs.child = f.tabID, f.session
		} else {
			return nil, &cdpError{Code: -32602, Message: "tab " + short(p.TargetID) + " is not handed to oko (only tabs given in the oko extension popup can be driven)"}
		}
		s.mu.Lock()
		s.nextSess++
		vs.id = fmt.Sprintf("OKO%04d%s", s.nextSess, strings.ToUpper(randomID()))
		s.sessions[vs.id] = vs
		s.mu.Unlock()
		return marshal(map[string]string{"sessionId": vs.id})
	case "Target.detachFromTarget":
		s.mu.Lock()
		vs := s.sessions[p.SessionID]
		if vs != nil && vs.client == c {
			delete(s.sessions, p.SessionID)
		} else {
			vs = nil
		}
		s.mu.Unlock()
		if vs == nil {
			return nil, &cdpError{Code: -32602, Message: "No session with given id"}
		}
		s.endSession(vs)
		return nil, nil
	case "Target.createTarget":
		s.mu.Lock()
		allow, up := s.allowNew, s.ext != nil
		s.mu.Unlock()
		if !up {
			return nil, errExtOffline
		}
		if !allow {
			if len(s.Status().Tabs) == 0 {
				return nil, errNoTab
			}
			return nil, &cdpError{Code: -32000, Message: "oko may not open tabs in your Chrome: turn on \"Let oko open its own tabs\" " +
				"in the oko extension popup, or hand over another tab"}
		}
		if why := restrictedURL(p.URL); why != "" {
			return nil, &cdpError{Code: -32000, Message: why}
		}
		res, err := s.extCall(ctx, extReq{Op: "createTab", URL: p.URL, NewWindow: p.NewWindow, Background: p.Background})
		if err != nil {
			return nil, err
		}
		var t Tab
		if err := json.Unmarshal(res, &t); err != nil || t.TargetID == "" {
			return nil, &cdpError{Code: -32000, Message: "the extension did not report the new tab"}
		}
		s.mu.Lock()
		if s.tabs[t.TabID] == nil {
			t.Own = true
			s.tabs[t.TabID] = &t
		}
		s.mu.Unlock()
		return marshal(map[string]string{"targetId": t.TargetID})
	case "Target.closeTarget":
		t := s.tabByTarget(p.TargetID)
		if t == nil {
			return nil, &cdpError{Code: -32602, Message: "No target with given id found"}
		}
		if _, err := s.extCall(ctx, extReq{Op: "closeTab", TabID: t.TabID}); err != nil {
			return nil, err
		}
		return marshal(map[string]bool{"success": true})
	case "Target.activateTarget":
		t := s.tabByTarget(p.TargetID)
		if t == nil {
			return nil, &cdpError{Code: -32602, Message: "No target with given id found"}
		}
		_, err := s.extCall(ctx, extReq{Op: "activateTab", TabID: t.TabID})
		return nil, err
	}
	return nil, notAvailable(req.Method)
}

func targetInfo(t Tab) map[string]interface{} {
	return map[string]interface{}{"targetId": t.TargetID, "type": "page", "title": t.Title, "url": t.URL,
		"attached": true, "canAccessOpener": false, "browserContextId": "oko-extension"}
}

func marshal(v interface{}) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return b, err
}

func short(id string) string {
	if len(id) > 6 {
		return strings.ToLower(id[:6])
	}
	return strings.ToLower(id)
}

func randomID() string {
	s, err := randomHex(8)
	if err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return s
}

func (s *Server) tabByTarget(id string) *Tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tabs {
		if t.TargetID == id {
			c := *t
			return &c
		}
	}
	return nil
}

// findFrame locates a child target (an out-of-process frame) of a handed-over
// tab. Chrome reports those only to a session that auto-attaches, so the
// bridge turns auto-attach on for the tabs and waits for the report.
func (s *Server) findFrame(ctx context.Context, targetID string) (frameRef, bool) {
	lookup := func() (frameRef, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		f, ok := s.frames[targetID]
		return f, ok
	}
	if f, ok := lookup(); ok {
		return f, true
	}
	params, _ := json.Marshal(map[string]bool{"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true})
	for _, t := range s.Status().Tabs {
		_, _ = s.extCall(ctx, extReq{Op: "cdp", TabID: t.TabID, Method: "Target.setAutoAttach", Params: params})
	}
	deadline := time.Now().Add(frameWait)
	for time.Now().Before(deadline) {
		if f, ok := lookup(); ok {
			return f, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return frameRef{}, false
}

// sessionCall relays a per-tab command to the extension.
func (s *Server) sessionCall(c *cdpClient, req cdpReq) (json.RawMessage, error) {
	s.mu.Lock()
	vs := s.sessions[req.SessionID]
	childTab, isChild := s.children[req.SessionID]
	s.mu.Unlock()
	er := extReq{Op: "cdp", Method: req.Method, Params: req.Params}
	switch {
	case vs != nil && vs.client == c:
		er.TabID, er.SessionID = vs.tabID, vs.child
	case isChild:
		er.TabID, er.SessionID = childTab, req.SessionID
	default:
		return nil, &cdpError{Code: -32001, Message: "Session with given id not found."}
	}
	if why := clientAllowed(req.Method, req.Params); why != "" {
		return nil, &cdpError{Code: -32601, Message: why}
	}
	ctx, cancel := context.WithTimeout(c.ctx, extCallTimeout)
	defer cancel()
	res, err := s.extCall(ctx, er)
	if err == nil && vs != nil {
		s.recordUndo(vs, req.Method, req.Params, res)
	}
	return res, err
}

// recordUndo remembers how to take back state a session set on the shared
// attachment.
func (s *Server) recordUndo(vs *vsession, method string, params, result json.RawMessage) {
	var p map[string]interface{}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	defer s.mu.Unlock()
	u := vs.undo
	switch method {
	case "Page.addScriptToEvaluateOnNewDocument":
		var r struct {
			Identifier string `json:"identifier"`
		}
		if json.Unmarshal(result, &r) == nil && r.Identifier != "" {
			u["script:"+r.Identifier] = undoCall{"Page.removeScriptToEvaluateOnNewDocument", map[string]string{"identifier": r.Identifier}}
		}
	case "Page.removeScriptToEvaluateOnNewDocument":
		if id, _ := p["identifier"].(string); id != "" {
			delete(u, "script:"+id)
		}
	case "Emulation.setDeviceMetricsOverride":
		u["metrics"] = undoCall{"Emulation.clearDeviceMetricsOverride", nil}
	case "Emulation.clearDeviceMetricsOverride":
		delete(u, "metrics")
	case "Page.startScreencast":
		u["screencast"] = undoCall{"Page.stopScreencast", nil}
	case "Page.stopScreencast":
		delete(u, "screencast")
	}
}

// idleDisables are sent when the last session on a tab ends, so the next oko
// command starts from a clean attachment, as it would on a fresh CDP session.
var idleDisables = []string{"Network.disable", "Log.disable", "DOMSnapshot.disable", "DOM.disable",
	"Performance.disable", "Runtime.disable", "Page.disable"}

// endSession undoes what the session left on the tab.
func (s *Server) endSession(vs *vsession) {
	s.mu.Lock()
	calls := make([]undoCall, 0, len(vs.undo))
	for _, u := range vs.undo {
		calls = append(calls, u)
	}
	last := vs.child == ""
	for _, o := range s.sessions {
		if o.tabID == vs.tabID && o.child == "" {
			last = false
			break
		}
	}
	_, live := s.tabs[vs.tabID]
	s.mu.Unlock()
	if !live {
		return
	}
	if last {
		for _, m := range idleDisables {
			calls = append(calls, undoCall{method: m})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, u := range calls {
		var params json.RawMessage
		if u.params != nil {
			params, _ = json.Marshal(u.params)
		}
		_, _ = s.extCall(ctx, extReq{Op: "cdp", TabID: vs.tabID, SessionID: vs.child, Method: u.method, Params: params})
	}
}

// dropClient ends every session of a client that went away.
func (s *Server) dropClient(c *cdpClient) {
	s.mu.Lock()
	delete(s.clients, c)
	var gone []*vsession
	for id, vs := range s.sessions {
		if vs.client == c {
			gone = append(gone, vs)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, vs := range gone {
		s.endSession(vs)
	}
}

// routeEvent hands a CDP event from a tab to every client session on it.
// Events of a child target (out-of-process frame) go to the client sessions
// attached to that child, under their own session ids.
func (s *Server) routeEvent(tabID int, child, method string, params json.RawMessage) {
	if child == "" {
		var p struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				TargetID string `json:"targetId"`
			} `json:"targetInfo"`
			TargetID string `json:"targetId"`
		}
		switch method {
		case "Target.attachedToTarget":
			if json.Unmarshal(params, &p) == nil && p.SessionID != "" {
				s.mu.Lock()
				s.children[p.SessionID] = tabID
				if p.TargetInfo.TargetID != "" {
					s.frames[p.TargetInfo.TargetID] = frameRef{tabID: tabID, session: p.SessionID}
				}
				s.mu.Unlock()
			}
		case "Target.detachedFromTarget":
			if json.Unmarshal(params, &p) == nil && p.SessionID != "" {
				s.mu.Lock()
				delete(s.children, p.SessionID)
				for id, f := range s.frames {
					if f.session == p.SessionID {
						delete(s.frames, id)
					}
				}
				s.mu.Unlock()
			}
		}
	}
	type out struct {
		c   *cdpClient
		sid string
	}
	var outs []out
	s.mu.Lock()
	for _, vs := range s.sessions {
		if vs.tabID == tabID && vs.child == child {
			outs = append(outs, out{vs.client, vs.id})
		}
	}
	s.mu.Unlock()
	for _, o := range outs {
		o.c.send(cdpEvent{Method: method, Params: params, SessionID: o.sid})
	}
}

// updateTabs takes the extension's list of handed-over tabs and tells
// clients what appeared, changed or went away.
func (s *Server) updateTabs(list []Tab, allowNew bool) {
	s.mu.Lock()
	s.allowNew = allowNew
	next := map[int]*Tab{}
	var events []cdpEvent
	var detached []struct {
		c *cdpClient
		e cdpEvent
	}
	for i := range list {
		t := list[i]
		if t.TargetID == "" {
			continue
		}
		next[t.TabID] = &t
		old := s.tabs[t.TabID]
		switch {
		case old == nil:
			events = append(events, cdpEvent{Method: "Target.targetCreated", Params: map[string]interface{}{"targetInfo": targetInfo(t)}})
		case old.URL != t.URL || old.Title != t.Title:
			events = append(events, cdpEvent{Method: "Target.targetInfoChanged", Params: map[string]interface{}{"targetInfo": targetInfo(t)}})
		}
	}
	for id, old := range s.tabs {
		if next[id] != nil {
			continue
		}
		for sid, vs := range s.sessions {
			if vs.tabID == id {
				delete(s.sessions, sid)
				detached = append(detached, struct {
					c *cdpClient
					e cdpEvent
				}{vs.client, cdpEvent{Method: "Target.detachedFromTarget", Params: map[string]string{"sessionId": sid, "targetId": old.TargetID}}})
			}
		}
		for sid, tab := range s.children {
			if tab == id {
				delete(s.children, sid)
			}
		}
		for fid, f := range s.frames {
			if f.tabID == id {
				delete(s.frames, fid)
			}
		}
		events = append(events, cdpEvent{Method: "Target.targetDestroyed", Params: map[string]string{"targetId": old.TargetID}})
	}
	s.tabs = next
	clients := make([]*cdpClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, d := range detached {
		d.c.send(d.e)
	}
	for _, c := range clients {
		for _, e := range events {
			c.send(e)
		}
	}
}
