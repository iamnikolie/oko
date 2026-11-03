package bridge

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
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
	Pub       string `json:"pub,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	Proof     string `json:"proof,omitempty"`
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
		var err error
		switch hello.Type {
		case "pair":
			err = s.pairHandshake(e, origin, hello)
		case "hello":
			err = s.helloHandshake(e, origin, hello)
		default:
			err = errors.New("expected hello or pair (an extension from before mutual authentication: reload it and pair again)")
		}
		if err != nil {
			refuse(err.Error())
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

// receive reads one message of the handshake.
func (e *extConn) receive(d time.Duration) (extMsg, error) {
	_ = e.ws.SetReadDeadline(time.Now().Add(d))
	var raw string
	if err := websocket.Message.Receive(e.ws, &raw); err != nil {
		return extMsg{}, errors.New("handshake: no answer")
	}
	var m extMsg
	err := json.Unmarshal([]byte(raw), &m)
	return m, err
}

// pairHandshake runs the one-time pairing (see crypto.go).
func (s *Server) pairHandshake(e *extConn, origin string, hello extMsg) error {
	if !validNonce(hello.Nonce) {
		return errors.New("bad pairing request")
	}
	pubExtBytes, err := hex.DecodeString(hello.Pub)
	if err != nil {
		return errors.New("bad pairing request")
	}
	pubExt, err := ecdh.P256().NewPublicKey(pubExtBytes)
	if err != nil {
		return errors.New("bad pairing request")
	}
	code, err := s.pair.beginPair(origin)
	if err != nil {
		return err
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	shared, err := priv.ECDH(pubExt)
	if err != nil {
		return errors.New("bad pairing request")
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	pubBridge := hex.EncodeToString(priv.PublicKey().Bytes())
	tr := pairTranscript(origin, hello.Pub, pubBridge, hello.Nonce, nonce)
	ck := codeKey(code, nonce)
	if err := e.send(map[string]string{"type": "pair-challenge", "pub": pubBridge, "nonce": nonce, "proof": hmacHex(ck, "bridge|"+tr)}); err != nil {
		return err
	}
	m, err := e.receive(2 * time.Minute)
	if err != nil {
		return err
	}
	if m.Type != "pair-confirm" || !hmacOK(ck, "ext|"+tr, m.Proof) {
		return errCodeWrong
	}
	if err := s.pair.completePair(code, hmacSum(shared, "oko-key|"+tr), origin); err != nil {
		return err
	}
	logf("paired with %s", origin)
	return e.send(map[string]string{"type": "paired"})
}

// helloHandshake authenticates a returning extension both ways.
func (s *Server) helloHandshake(e *extConn, origin string, hello extMsg) error {
	key, ok := s.pair.key(origin)
	if !ok {
		if s.pair.Paired() == "" {
			return errors.New("not paired: run 'oko ext pair' and enter the code in the popup")
		}
		return errors.New("not the paired extension: run 'oko ext pair' and enter the code in the popup")
	}
	if !validNonce(hello.Nonce) {
		return errors.New("bad hello")
	}
	nonce, err := randomHex(16)
	if err != nil {
		return err
	}
	if err := e.send(map[string]string{"type": "challenge", "nonce": nonce, "proof": hmacHex(key, helloBridgeMsg(origin, hello.Nonce, nonce))}); err != nil {
		return err
	}
	m, err := e.receive(helloTimeout)
	if err != nil {
		return err
	}
	if m.Type != "auth" || !hmacOK(key, helloExtMsg(origin, hello.Nonce, nonce), m.Proof) {
		return errors.New("extension could not prove the pairing: run 'oko ext pair' and enter the code in the popup")
	}
	return e.send(map[string]string{"type": "welcome"})
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
