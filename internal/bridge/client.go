package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Client is the oko CLI's handle on the bridge.
type Client struct {
	Dir    string
	Port   int
	secret string
}

// NewClient prepares a client for the bridge whose state lives under home.
func NewClient(home string) (*Client, error) {
	dir := Dir(home)
	secret, err := ClientSecret(dir)
	if err != nil {
		return nil, err
	}
	return &Client{Dir: dir, Port: Port(), secret: secret}, nil
}

// CDPURL is the websocket URL oko commands attach to instead of Chrome's.
func (c *Client) CDPURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/cdp/%s", c.Port, c.secret)
}

func (c *Client) do(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/control/%s", c.Port, path), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("X-Oko-Secret", c.secret)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("port %d answers but refuses oko's secret: another oko home or another program holds it (set $OKO_EXT_PORT)", c.Port)
		}
		return fmt.Errorf("bridge: %s", strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Status asks the running bridge for its state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, "status", &st)
	return st, err
}

// Running reports whether the bridge answers.
func (c *Client) Running(ctx context.Context) bool {
	_, err := c.Status(ctx)
	return err == nil
}

// Ensure starts the bridge (a detached "oko _bridge") unless it runs.
func (c *Client) Ensure(ctx context.Context) error {
	if _, err := c.Status(ctx); err == nil {
		return nil
	} else if !isConnRefused(err) {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(c.Dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	cmd := exec.Command(self, "_bridge")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := c.Status(ctx); err == nil {
			return nil
		}
	}
	return fmt.Errorf("bridge did not start; see %s", LogPath(c.Dir))
}

func isConnRefused(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) || strings.Contains(err.Error(), "connection refused")
}

// WaitExtension waits until the extension is connected and returns the
// bridge's state.
func (c *Client) WaitExtension(ctx context.Context) (Status, error) {
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return st, err
		}
		if st.Connected {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// NewCode opens a pairing code.
func (c *Client) NewCode(ctx context.Context) (string, error) {
	var r struct {
		Code string `json:"code"`
	}
	err := c.do(ctx, "pair", &r)
	return r.Code, err
}

// ReleaseAll asks the extension to give every tab back.
func (c *Client) ReleaseAll(ctx context.Context) error { return c.do(ctx, "release", nil) }

// Unpair forgets the paired extension.
func (c *Client) Unpair(ctx context.Context) error { return c.do(ctx, "unpair", nil) }

// Stop ends the bridge.
func (c *Client) Stop(ctx context.Context) error { return c.do(ctx, "stop", nil) }

// Serve runs the bridge in this process (the hidden "oko _bridge").
func Serve(home string) error {
	dir := Dir(home)
	secret, err := ClientSecret(dir)
	if err != nil {
		return err
	}
	port := Port()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w (set $OKO_EXT_PORT to use another port, and the same port in the extension popup)", port, err)
	}
	_ = os.WriteFile(pidPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer func() { _ = os.Remove(pidPath(dir)) }()
	logf("bridge on 127.0.0.1:%d", port)
	return NewServer(port, secret, LoadPairing(pairingPath(dir))).Serve(ln)
}
