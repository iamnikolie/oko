package chrome

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

// Chrome takes a proxy only as host:port: no credentials on the command line,
// and answering auth challenges over CDP would need a connection that lives
// as long as the browser. So a proxied profile points Chrome at a small local
// relay (a detached "oko _proxy" process) that adds the upstream credentials.
// The relay re-reads the profile's proxy for every new connection, so
// switching the upstream (another exit IP) needs no browser restart.

// ParseProxy normalizes a proxy URL: "host:port" means http, and the scheme
// must be one oko can relay.
func ParseProxy(s string) (*url.URL, error) {
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("bad proxy URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy scheme %q not supported (http, https, socks5)", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, errors.New("proxy URL needs host and port, e.g. http://user:pass@host:8080")
	}
	return u, nil
}

// RedactProxy prints a proxy URL with its password masked.
func RedactProxy(s string) string {
	u, err := ParseProxy(s)
	if err != nil {
		return "<invalid>"
	}
	if _, ok := u.User.Password(); ok {
		return u.Scheme + "://" + url.User(u.User.Username()).String() + ":***@" + u.Host
	}
	return u.String()
}

func (p *Profile) relayPortPath() string { return filepath.Join(p.Dir, "proxy.port") }
func (p *Profile) RelayLogPath() string  { return filepath.Join(p.Dir, "proxy.log") }

// RelayPort returns the port of this profile's live relay, or 0.
func (p *Profile) RelayPort() int {
	if p.State.Relay <= 0 || syscall.Kill(p.State.Relay, 0) != nil {
		return 0
	}
	b, err := os.ReadFile(p.relayPortPath())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if port <= 0 {
		return 0
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return 0
	}
	c.Close()
	return port
}

// ensureRelay starts the profile's relay unless one is running, and returns
// its port.
func (p *Profile) ensureRelay() (int, error) {
	if port := p.RelayPort(); port > 0 {
		return port, nil
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	_ = os.Remove(p.relayPortPath())
	logf, err := os.OpenFile(p.RelayLogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	c := exec.Command(self, "_proxy", "--profile", p.Name)
	c.Stdout, c.Stderr = logf, logf
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, fmt.Errorf("start proxy relay: %w", err)
	}
	go func() { _ = c.Wait() }()
	p.State.Relay = c.Process.Pid
	_ = p.Save()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if port := p.RelayPort(); port > 0 {
			return port, nil
		}
	}
	return 0, fmt.Errorf("proxy relay did not start; see %s", p.RelayLogPath())
}

// StopRelay ends the profile's relay, if any.
func (p *Profile) StopRelay() {
	if p.State.Relay > 0 && syscall.Kill(p.State.Relay, 0) == nil {
		_ = syscall.Kill(p.State.Relay, syscall.SIGTERM)
	}
	p.State.Relay = 0
	_ = p.Save()
}

// ServeRelay runs the relay for the named profile until its Chrome is gone.
func ServeRelay(name string) error {
	p, err := Load(name)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(p.relayPortPath(), []byte(strconv.Itoa(port)), 0o600); err != nil {
		return err
	}
	r := &relay{name: name}
	go r.watch(ln)
	log.Printf("relay on 127.0.0.1:%d", port)
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second}
	err = srv.Serve(ln)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

type relay struct {
	name string
	mu   sync.Mutex
	raw  string
	up   *url.URL // nil: direct
	tr   *http.Transport
}

// watch closes the listener once the profile's Chrome has been gone for a
// while (a grace period covers the launch itself).
func (r *relay) watch(ln net.Listener) {
	time.Sleep(30 * time.Second)
	gone := 0
	for {
		p, err := Load(r.name)
		if err == nil && p.PID() != 0 {
			gone = 0
		} else if gone++; gone >= 3 {
			log.Printf("chrome gone, relay exits")
			ln.Close()
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// upstream returns the current upstream proxy (nil for direct) and the
// transport for plain-HTTP requests through it.
func (r *relay) upstream() (*url.URL, *http.Transport) {
	p, err := Load(r.name)
	raw := ""
	if err == nil {
		raw = p.State.Proxy
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tr != nil && raw == r.raw {
		return r.up, r.tr
	}
	r.raw, r.up = raw, nil
	if raw != "" {
		if u, err := ParseProxy(raw); err == nil {
			r.up = u
		} else {
			log.Printf("profile proxy invalid, going direct: %v", err)
		}
	}
	if r.tr != nil {
		r.tr.CloseIdleConnections()
	}
	r.tr = &http.Transport{Proxy: http.ProxyURL(r.up), ForceAttemptHTTP2: false, IdleConnTimeout: 60 * time.Second}
	if r.up != nil {
		log.Printf("upstream %s", RedactProxy(raw))
	} else {
		log.Printf("upstream direct")
	}
	return r.up, r.tr
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	up, tr := r.upstream()
	if req.Method == http.MethodConnect {
		r.tunnel(w, req, up)
		return
	}
	if !req.URL.IsAbs() {
		http.Error(w, "oko proxy relay: absolute URL expected", http.StatusBadRequest)
		return
	}
	out := req.Clone(req.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		log.Printf("%s %s: %v", req.Method, req.URL.Host, err)
		http.Error(w, "oko proxy relay: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (r *relay) tunnel(w http.ResponseWriter, req *http.Request, up *url.URL) {
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	dst, rd, err := dialVia(ctx, up, req.Host)
	if err != nil {
		log.Printf("CONNECT %s: %v", req.Host, err)
		http.Error(w, "oko proxy relay: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	src, buf, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		// Bytes the browser already sent past the CONNECT head sit in buf.
		_, _ = io.Copy(dst, buf)
		closeWrite(dst)
	}()
	_, _ = io.Copy(src, rd)
	src.Close()
	dst.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		c.Close()
	}
}

// dialVia opens a tunnel to addr through the upstream proxy (or directly).
// The reader returned must be used for reading: it may hold bytes the
// upstream sent right after its CONNECT reply.
func dialVia(ctx context.Context, up *url.URL, addr string) (net.Conn, io.Reader, error) {
	d := &net.Dialer{Timeout: 15 * time.Second}
	if up == nil {
		c, err := d.DialContext(ctx, "tcp", addr)
		return c, c, err
	}
	switch up.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if up.User != nil {
			pw, _ := up.User.Password()
			auth = &proxy.Auth{User: up.User.Username(), Password: pw}
		}
		sd, err := proxy.SOCKS5("tcp", up.Host, auth, d)
		if err != nil {
			return nil, nil, err
		}
		c, err := sd.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
		return c, c, err
	}
	c, err := d.DialContext(ctx, "tcp", up.Host)
	if err != nil {
		return nil, nil, err
	}
	if up.Scheme == "https" {
		tc := tls.Client(c, &tls.Config{ServerName: up.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, nil, err
		}
		c = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	head := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if up.User != nil {
		pw, _ := up.User.Password()
		head += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(up.User.Username()+":"+pw)) + "\r\n"
	}
	if _, err := c.Write([]byte(head + "\r\n")); err != nil {
		c.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, nil, fmt.Errorf("upstream proxy answered %s", resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	return c, br, nil
}
