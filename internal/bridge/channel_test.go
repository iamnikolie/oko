package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func testChannels() (bridge, ext *channel) {
	link := bytes.Repeat([]byte{7}, 32)
	ne, nb := strings.Repeat("a", 32), strings.Repeat("b", 32)
	return newChannel(link, testOrigin, ne, nb, true), newChannel(link, testOrigin, ne, nb, false)
}

func TestChannel(t *testing.T) {
	br, ex := testChannels()
	for i, body := range []string{`{"op":"cdp","id":1}`, `{"id":2,"result":{"s":"a b  c"}}`, `{}`} {
		got, err := ex.open(br.seal([]byte(body)))
		if err != nil || string(got) != body {
			t.Fatalf("bridge → ext frame %d: %q %v", i, got, err)
		}
		got, err = br.open(ex.seal([]byte(body)))
		if err != nil || string(got) != body {
			t.Fatalf("ext → bridge frame %d: %q %v", i, got, err)
		}
	}

	// Each case starts from a fresh pair of channels; frames that must fail
	// are fed to the extension's end after any genuine frames before them.
	type attack struct {
		name  string
		build func(br, ex *channel) (genuine []string, bad string)
	}
	for _, a := range []attack{
		{"tampered body", func(br, ex *channel) ([]string, string) {
			f := br.seal([]byte(`{"op":"cdp","method":"Page.reload"}`))
			return nil, strings.Replace(f, "Page.reload", "Runtime.evaluate", 1)
		}},
		{"tampered mac", func(br, ex *channel) ([]string, string) {
			f := []byte(br.seal([]byte(`{}`)))
			f[5] ^= 1
			return nil, string(f)
		}},
		{"tampered counter", func(br, ex *channel) ([]string, string) {
			f := br.seal([]byte(`{}`))
			return nil, "2" + f[1:]
		}},
		{"replayed", func(br, ex *channel) ([]string, string) {
			f := br.seal([]byte(`{"op":"releaseAll","id":1}`))
			return []string{f}, f
		}},
		{"reordered", func(br, ex *channel) ([]string, string) {
			_ = br.seal([]byte(`{"id":1}`))
			return nil, br.seal([]byte(`{"id":2}`))
		}},
		{"reflected", func(br, ex *channel) ([]string, string) {
			return nil, ex.seal([]byte(`{"id":1}`)) // the extension's own frame sent back to it
		}},
		{"unsealed (relay injection)", func(br, ex *channel) ([]string, string) {
			return nil, `{"id":99,"op":"cdp","tabId":1,"method":"Runtime.evaluate","params":{"expression":"6*7"}}`
		}},
		{"forged without the key", func(br, ex *channel) ([]string, string) {
			return nil, "1 " + strings.Repeat("0", 64) + ` {"id":99,"op":"releaseAll"}`
		}},
		{"other session", func(br, ex *channel) ([]string, string) {
			other := newChannel(bytes.Repeat([]byte{7}, 32), testOrigin, strings.Repeat("c", 32), strings.Repeat("b", 32), true)
			return nil, other.seal([]byte(`{"id":1}`))
		}},
		{"leading zero", func(br, ex *channel) ([]string, string) {
			f := br.seal([]byte(`{}`))
			return nil, "0" + f
		}},
		{"malformed", func(br, ex *channel) ([]string, string) { return nil, "1" }},
	} {
		t.Run(a.name, func(t *testing.T) {
			br, ex := testChannels()
			genuine, bad := a.build(br, ex)
			for _, f := range genuine {
				if _, err := ex.open(f); err != nil {
					t.Fatalf("genuine frame refused: %v", err)
				}
			}
			if body, err := ex.open(bad); err == nil {
				t.Fatalf("accepted %q", body)
			}
		})
	}
}

// TestExtLinkRejectsBadFrames: a program holding the socket after a genuine
// handshake (a relay that forwarded it) cannot get a frame of its own past the
// bridge; the first bad frame ends the session and is not acted on.
func TestExtLinkRejectsBadFrames(t *testing.T) {
	tabsFrame := func(url string) []byte {
		b, _ := json.Marshal(map[string]interface{}{"ev": "tabs", "allowNew": true,
			"tabs": []Tab{{TabID: 7, TargetID: "T7", URL: url}}})
		return b
	}
	for _, tc := range []struct {
		name string
		// frames returns what goes on the wire after the handshake.
		frames func(e *fakeExt) []string
	}{
		{"injected", func(e *fakeExt) []string { return []string{string(tabsFrame("https://injected.example/"))} }},
		{"tampered", func(e *fakeExt) []string {
			return []string{strings.Replace(e.ch.seal(tabsFrame("https://genuine.example/")), "genuine", "injected", 1)}
		}},
		{"replayed", func(e *fakeExt) []string {
			f := e.ch.seal(tabsFrame("https://genuine.example/"))
			return []string{f, f} // the copy must end the session
		}},
		{"reordered", func(e *fakeExt) []string {
			_ = e.ch.seal(tabsFrame("https://genuine.example/"))
			return []string{e.ch.seal(tabsFrame("https://injected.example/"))}
		}},
		{"forged", func(e *fakeExt) []string {
			return []string{"1 " + strings.Repeat("0", 64) + " " + string(tabsFrame("https://injected.example/"))}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBridge(t)
			code, _ := b.srv.pair.NewCode()
			key, pe, _, err := extPair(t, b, testOrigin, code)
			if err != nil {
				t.Fatal(err)
			}
			_ = pe.ws.Close()
			e, _, err := extHello(t, b, testOrigin, key)
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, func() bool { return b.srv.Status().Connected })
			frames := tc.frames(e)
			for _, f := range frames {
				if err := websocket.Message.Send(e.ws, f); err != nil {
					break // already closed
				}
			}
			_ = e.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			var rest string
			if err := websocket.Message.Receive(e.ws, &rest); err == nil {
				t.Fatalf("link still open, got %q", rest)
			}
			waitUntil(t, func() bool { return !b.srv.Status().Connected })
			for _, tab := range b.srv.Status().Tabs {
				if strings.Contains(tab.URL, "injected") {
					t.Fatalf("bridge acted on a bad frame: %+v", tab)
				}
			}
		})
	}
}

// TestExtLinkFramesBothWays: after the handshake the bridge seals what it
// sends for the extension's direction (requests and refusals alike) and
// accepts the extension's sealed answers.
func TestExtLinkFramesBothWays(t *testing.T) {
	b := newTestBridge(t)
	code, _ := b.srv.pair.NewCode()
	_, e, _, err := extPair(t, b, testOrigin, code)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return b.srv.Status().Connected })
	done := make(chan error, 1)
	go func() {
		_, err := b.srv.extCall(context.Background(), extReq{Op: "releaseAll"})
		done <- err
	}()
	_ = e.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var req extReq
	if err := e.recv(&req); err != nil || req.Op != "releaseAll" {
		t.Fatalf("request: %+v %v", req, err)
	}
	if err := e.send(map[string]interface{}{"id": req.ID, "result": map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("sealed answer refused: %v", err)
	}
	b.srv.dropExt("unpaired")
	var m map[string]string
	if err := e.recv(&m); err != nil || m["type"] != "refused" {
		t.Fatalf("refusal: %v %v", m, err)
	}
}
