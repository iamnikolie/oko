package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// maxMessage bounds one websocket message (a full-page screenshot is a few MB
// of base64).
const maxMessage = 128 << 20

// extCallTimeout bounds one request to the extension when the caller set no
// deadline of its own.
const extCallTimeout = 60 * time.Second

// Tab is a tab the user handed to oko (or oko opened with the user's leave).
type Tab struct {
	TabID    int    `json:"tabId"`
	TargetID string `json:"targetId"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	// Own marks a tab oko opened itself (only with the popup's opt-in).
	Own bool `json:"own"`
}

// Status is what 'oko ext status' and 'oko status' show.
type Status struct {
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Paired    bool   `json:"paired"`
	Origin    string `json:"origin,omitempty"`
	CodeOpen  bool   `json:"code_open"`
	Connected bool   `json:"connected"`
	Version   string `json:"extension_version,omitempty"`
	Browser   string `json:"browser,omitempty"`
	AllowNew  bool   `json:"allow_new_tabs"`
	Tabs      []Tab  `json:"tabs"`
	Clients   int    `json:"clients"`
}

// Server is the bridge between oko commands and the extension.
type Server struct {
	port   int
	secret string
	pair   *Pairing

	mu        sync.Mutex
	ext       *extConn
	tabs      map[int]*Tab
	allowNew  bool
	version   string
	product   string
	userAgent string
	clients   map[*cdpClient]bool
	sessions  map[string]*vsession // virtual session id → session
	children  map[string]int       // child session id (frames, workers) → tab id
	frames    map[string]frameRef  // child target id → where it hangs
	nextSess  int

	// CLI authentication: pending handshakes by bridge nonce, and tickets.
	authPending map[string]pendingAuth
	tickets     map[string]time.Time // ticket → expiry
}

type pendingAuth struct {
	nonceCLI string
	exp      time.Time
}

const (
	// ticketTTL is how long a CLI ticket lives after its last use.
	ticketTTL = 15 * time.Minute
	// authTTL bounds a half-done CLI handshake.
	authTTL = 30 * time.Second
	// maxPending and maxTickets bound the tables against a local flood.
	maxPending = 64
	maxTickets = 256
)

// NewServer prepares a bridge on port with the given client secret and
// pairing store.
func NewServer(port int, secret string, pair *Pairing) *Server {
	return &Server{
		port: port, secret: secret, pair: pair,
		tabs:     map[int]*Tab{},
		clients:  map[*cdpClient]bool{},
		sessions: map[string]*vsession{},
		children: map[string]int{},
		frames:   map[string]frameRef{},

		authPending: map[string]pendingAuth{},
		tickets:     map[string]time.Time{},
	}
}

// Serve runs the bridge on the listener until it is closed.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	err := srv.Serve(ln)
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler routes the bridge's endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/ext", s.guard(false, s.extHandler()))
	mux.Handle("/cdp/", s.guard(true, s.cdpHandler()))
	mux.Handle("/control/", s.guard(true, http.HandlerFunc(s.control)))
	return mux
}

// guard admits only loopback requests addressed to the bridge by name
// (DNS-rebinding defence). Client endpoints also refuse any request that
// carries an Origin: browsers always send one, oko commands never do, so a web
// page cannot reach them even if it learnt the secret.
func (s *Server) guard(client bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host != "127.0.0.1:"+strconv.Itoa(s.port) && host != "localhost:"+strconv.Itoa(s.port) {
			http.Error(w, "oko bridge: wrong host", http.StatusForbidden)
			return
		}
		if ip, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(ip).IsLoopback() {
			http.Error(w, "oko bridge: loopback only", http.StatusForbidden)
			return
		}
		if client {
			if r.Header.Get("Origin") != "" {
				http.Error(w, "oko bridge: browsers may not use this endpoint", http.StatusForbidden)
				return
			}
			if !strings.HasPrefix(r.URL.Path, "/control/auth/") && !s.ticketOK(r) {
				http.Error(w, "oko bridge: no valid ticket", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// ticketOK checks the ticket a CLI got from the handshake (header, or the
// /cdp/<ticket> path) and extends its life.
func (s *Server) ticketOK(r *http.Request) bool {
	t := r.Header.Get("X-Oko-Ticket")
	if strings.HasPrefix(r.URL.Path, "/cdp/") {
		t = strings.TrimPrefix(r.URL.Path, "/cdp/")
	}
	if t == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range s.tickets {
		if constEq(k, t) {
			if time.Now().After(exp) {
				delete(s.tickets, k)
				return false
			}
			s.tickets[k] = time.Now().Add(ticketTTL)
			return true
		}
	}
	return false
}

// authStart is the bridge's half of the CLI handshake: it proves it holds
// the client secret before the CLI proves anything.
func (s *Server) authStart(nonceCLI string) (map[string]string, error) {
	if !validNonce(nonceCLI) {
		return nil, errors.New("bad nonce")
	}
	nonce, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	now := time.Now()
	for k, p := range s.authPending {
		if now.After(p.exp) {
			delete(s.authPending, k)
		}
	}
	if len(s.authPending) >= maxPending {
		s.mu.Unlock()
		return nil, errors.New("too many handshakes in flight")
	}
	s.authPending[nonce] = pendingAuth{nonceCLI: nonceCLI, exp: now.Add(authTTL)}
	s.mu.Unlock()
	return map[string]string{"nonce": nonce, "proof": hmacHex([]byte(s.secret), cliBridgeMsg(nonceCLI, nonce))}, nil
}

// authFinish checks the CLI's proof and hands out a ticket.
func (s *Server) authFinish(nonce, proof string) (map[string]string, error) {
	s.mu.Lock()
	p, ok := s.authPending[nonce]
	delete(s.authPending, nonce)
	s.mu.Unlock()
	if !ok || time.Now().After(p.exp) || !hmacOK([]byte(s.secret), cliClientMsg(p.nonceCLI, nonce), proof) {
		return nil, errors.New("bad proof")
	}
	ticket, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	now := time.Now()
	for k, exp := range s.tickets {
		if now.After(exp) {
			delete(s.tickets, k)
		}
	}
	if len(s.tickets) >= maxTickets {
		var oldest string
		for k, exp := range s.tickets {
			if oldest == "" || exp.Before(s.tickets[oldest]) {
				oldest = k
			}
		}
		delete(s.tickets, oldest)
	}
	s.tickets[ticket] = now.Add(ticketTTL)
	s.mu.Unlock()
	return map[string]string{"ticket": ticket}, nil
}

// control is the small HTTP API the oko CLI uses.
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "oko bridge: POST only", http.StatusMethodNotAllowed)
		return
	}
	write := func(v interface{}, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	var body struct {
		Nonce string `json:"nonce"`
		Proof string `json:"proof"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	switch strings.TrimPrefix(r.URL.Path, "/control/") {
	case "auth/start":
		res, err := s.authStart(body.Nonce)
		if err != nil {
			http.Error(w, "oko bridge: "+err.Error(), http.StatusBadRequest)
			return
		}
		write(res, nil)
	case "auth/finish":
		res, err := s.authFinish(body.Nonce, body.Proof)
		if err != nil {
			http.Error(w, "oko bridge: "+err.Error(), http.StatusUnauthorized)
			return
		}
		write(res, nil)
	case "status":
		write(s.Status(), nil)
	case "pair":
		code, err := s.pair.NewCode()
		write(map[string]string{"code": code}, err)
	case "unpair":
		err := s.pair.Unpair()
		s.dropExt("unpaired")
		write(map[string]bool{"ok": true}, err)
	case "release":
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		_, err := s.extCall(ctx, extReq{Op: "releaseAll"})
		write(map[string]bool{"ok": err == nil}, err)
	case "stop":
		write(map[string]bool{"ok": true}, nil)
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}()
	default:
		http.NotFound(w, r)
	}
}

// Status reports pairing, the extension link and the tabs oko may drive.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Port: s.port, PID: os.Getpid(), Origin: s.pair.Paired(), CodeOpen: s.pair.CodeOpen(),
		Connected: s.ext != nil, AllowNew: s.allowNew, Version: s.version, Browser: s.product, Clients: len(s.clients),
		Tabs: s.tabListLocked()}
	st.Paired = st.Origin != ""
	return st
}

func (s *Server) tabListLocked() []Tab {
	out := make([]Tab, 0, len(s.tabs))
	for _, t := range s.tabs {
		out = append(out, *t)
	}
	sortTabs(out)
	return out
}

func sortTabs(ts []Tab) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].TabID < ts[j-1].TabID; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

var errExtOffline = errors.New("the oko extension is not connected (is Chrome running with the extension paired? its popup shows the link)")

func newWS(h func(*websocket.Conn)) websocket.Server {
	return websocket.Server{
		// The guard has already checked host, loopback and Origin.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(c *websocket.Conn) {
			c.MaxPayloadBytes = maxMessage
			h(c)
		},
	}
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func logf(format string, args ...interface{}) { log.Printf(format, args...) }
