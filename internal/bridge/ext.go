package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// helloTimeout is how long a fresh /ext connection has to say who it is.
const helloTimeout = 10 * time.Second

// extConn is the live link to the paired extension.
type extConn struct {
	ws     *websocket.Conn
	sendMu sync.Mutex

	mu      sync.Mutex
	next    int64
	pending map[int64]chan extMsg
	closed  chan struct{}
	once    sync.Once
}

// extReq is a request to the extension. Ops: cdp (chrome.debugger.sendCommand
// on a handed-over tab), createTab, closeTab, activateTab, releaseAll.
type extReq struct {
	ID         int64           `json:"id"`
	Op         string          `json:"op"`
	TabID      int             `json:"tabId,omitempty"`
	SessionID  string          `json:"sessionId,omitempty"`
	Method     string          `json:"method,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	URL        string          `json:"url,omitempty"`
	NewWindow  bool            `json:"newWindow,omitempty"`
	Background bool            `json:"background,omitempty"`
}

type extError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

// extMsg is anything the extension sends: a hello, a reply (id) or an event
// (ev).
type extMsg struct {
	Type      string `json:"type,omitempty"`
	Token     string `json:"token,omitempty"`
	Code      string `json:"code,omitempty"`
	Version   string `json:"version,omitempty"`
	Product   string `json:"product,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`

	ID     int64           `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *extError       `json:"error,omitempty"`

	Ev        string          `json:"ev,omitempty"`
	TabID     int             `json:"tabId,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Tabs      []Tab           `json:"tabs,omitempty"`
	AllowNew  bool            `json:"allowNew,omitempty"`
}

func (e *extConn) send(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	_ = e.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return websocket.Message.Send(e.ws, string(b))
}

func (e *extConn) close() {
	e.once.Do(func() {
		close(e.closed)
		_ = e.ws.Close()
		e.mu.Lock()
		for id, ch := range e.pending {
			ch <- extMsg{ID: id, Error: &extError{Message: errExtOffline.Error()}}
			delete(e.pending, id)
		}
		e.mu.Unlock()
	})
}

func (s *Server) extHandler() websocket.Server {
	return newWS(func(ws *websocket.Conn) {
		origin := ws.Request().Header.Get("Origin")
		_ = ws.SetReadDeadline(time.Now().Add(helloTimeout))
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			_ = ws.Close()
			return
		}
		var hello extMsg
		_ = json.Unmarshal([]byte(raw), &hello)
		e := &extConn{ws: ws, pending: map[int64]chan extMsg{}, closed: make(chan struct{})}
		refuse := func(reason string) {
			logf("extension refused (%s): %s", origin, reason)
			_ = e.send(map[string]string{"type": "refused", "reason": reason})
			_ = ws.Close()
		}
		switch hello.Type {
		case "pair":
			token, err := s.pair.Pair(hello.Code, origin)
			if err != nil {
				refuse(err.Error())
				return
			}
			logf("paired with %s", origin)
			if err := e.send(map[string]string{"type": "paired", "token": token}); err != nil {
				return
			}
		case "hello":
			if !s.pair.Verify(hello.Token, origin) {
				if s.pair.Paired() == "" {
					refuse("not paired: run 'oko ext pair' and enter the code in the popup")
				} else {
					refuse("unknown extension or stale token: run 'oko ext pair' and enter the code in the popup")
				}
				return
			}
			if err := e.send(map[string]string{"type": "welcome"}); err != nil {
				return
			}
		default:
			refuse("expected hello or pair")
			return
		}
		_ = ws.SetReadDeadline(time.Time{})
		s.attachExt(e, hello)
		defer s.detachExt(e)
		for {
			var msg string
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				return
			}
			var m extMsg
			if err := json.Unmarshal([]byte(msg), &m); err != nil {
				continue
			}
			s.fromExt(e, m)
		}
	})
}

func (s *Server) attachExt(e *extConn, hello extMsg) {
	s.mu.Lock()
	old := s.ext
	s.ext = e
	s.version, s.product, s.userAgent = hello.Version, hello.Product, hello.UserAgent
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	logf("extension connected (version %s, %s)", hello.Version, hello.Product)
}

func (s *Server) detachExt(e *extConn) {
	e.close()
	s.mu.Lock()
	if s.ext == e {
		s.ext = nil
		logf("extension disconnected")
	}
	s.mu.Unlock()
}

// dropExt closes the extension link (after unpairing).
func (s *Server) dropExt(reason string) {
	s.mu.Lock()
	e := s.ext
	s.mu.Unlock()
	if e != nil {
		_ = e.send(map[string]string{"type": "refused", "reason": reason})
		e.close()
	}
}

func (s *Server) fromExt(e *extConn, m extMsg) {
	switch {
	case m.ID != 0 && m.Ev == "":
		e.mu.Lock()
		ch := e.pending[m.ID]
		delete(e.pending, m.ID)
		e.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	case m.Ev == "tabs":
		s.updateTabs(m.Tabs, m.AllowNew)
	case m.Ev == "cdp":
		s.routeEvent(m.TabID, m.SessionID, m.Method, m.Params)
	}
}

// extCall sends one request to the extension and waits for its answer.
func (s *Server) extCall(ctx context.Context, req extReq) (json.RawMessage, error) {
	s.mu.Lock()
	e := s.ext
	s.mu.Unlock()
	if e == nil {
		return nil, errExtOffline
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, extCallTimeout)
		defer cancel()
	}
	ch := make(chan extMsg, 1)
	e.mu.Lock()
	e.next++
	req.ID = e.next
	e.pending[req.ID] = ch
	e.mu.Unlock()
	if err := e.send(req); err != nil {
		e.mu.Lock()
		delete(e.pending, req.ID)
		e.mu.Unlock()
		return nil, errExtOffline
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, &cdpError{Code: orDefault(m.Error.Code, -32000), Message: m.Error.Message}
		}
		if len(m.Result) == 0 || string(m.Result) == "null" {
			return json.RawMessage("{}"), nil
		}
		return m.Result, nil
	case <-ctx.Done():
		e.mu.Lock()
		delete(e.pending, req.ID)
		e.mu.Unlock()
		return nil, errors.New("the oko extension did not answer in time")
	case <-e.closed:
		return nil, errExtOffline
	}
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
