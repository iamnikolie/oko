package e2e

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeProxy is an upstream HTTP proxy that requires Basic auth user:pass,
// answers plain-HTTP requests itself with its name, and tunnels every CONNECT
// to tlsAddr.
type fakeProxy struct {
	name, tlsAddr string
	mu            sync.Mutex
	connects      []string
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if r.Header.Get("Proxy-Authorization") != want {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fake"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect {
		fmt.Fprintf(w, "<html><title>via %s</title><body><p>served by %s for %s</p></body></html>", f.name, f.name, r.URL.Host)
		return
	}
	f.mu.Lock()
	f.connects = append(f.connects, r.Host)
	f.mu.Unlock()
	dst, err := net.Dial("tcp", f.tlsAddr)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	src, buf, _ := w.(http.Hijacker).Hijack()
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() { _, _ = io.Copy(dst, buf); dst.Close() }()
	_, _ = io.Copy(src, dst)
	src.Close()
}

func TestProxy(t *testing.T) {
	tlsSrv := httptest.NewUnstartedServer(http.NotFoundHandler())
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0) // Chrome rejecting the test cert
	tlsSrv.StartTLS()
	defer tlsSrv.Close()
	a := &fakeProxy{name: "proxy-A", tlsAddr: tlsSrv.Listener.Addr().String()}
	b := &fakeProxy{name: "proxy-B", tlsAddr: tlsSrv.Listener.Addr().String()}
	pa := strings.TrimPrefix(serve("127.0.0.1", a), "http://")
	pb := strings.TrimPrefix(serve("127.0.0.1", b), "http://")

	const prof = "proxied"
	defer oko(t, "--profile", prof, "down")
	out := oko(t, "--profile", prof, "up", "--headless", "--proxy", "http://user:pass@"+pa)
	must(t, out, "proxy: http://user:***@"+pa+" via relay")
	mustNot(t, out, "pass@")

	// Plain HTTP to a name only the proxy knows; credentials added by the relay.
	oko(t, "--profile", prof, "open", "http://oko-proxy.test/page")
	must(t, oko(t, "--profile", prof, "read"), "served by proxy-A for oko-proxy.test")

	// HTTPS goes through CONNECT; the certificate is the test server's own,
	// so reaching a TLS handshake at all proves the tunnel.
	out = oko(t, "!", "--profile", prof, "open", "https://oko-proxy.test/")
	must(t, out, "ERR_CERT")
	a.mu.Lock()
	must(t, strings.Join(a.connects, " "), "oko-proxy.test:443")
	a.mu.Unlock()

	// Switching the upstream applies to new connections without a restart.
	must(t, oko(t, "--profile", prof, "up", "--proxy", "http://user:pass@"+pb), "(new connections)")
	oko(t, "--profile", prof, "open", "http://oko-proxy2.test/page")
	must(t, oko(t, "--profile", prof, "read"), "served by proxy-B for oko-proxy2.test")

	// Rejected credentials surface the upstream's answer, not just Chrome's code.
	oko(t, "--profile", prof, "up", "--proxy", "http://user:wrong@"+pa)
	out = oko(t, "!", "--profile", prof, "open", "https://oko-proxy3.test/")
	must(t, out, "ERR_TUNNEL_CONNECTION_FAILED", "407")

	must(t, oko(t, "--profile", prof, "status"), "proxy: http://user:***@"+pa)
}
