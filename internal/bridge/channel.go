package bridge

import (
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// A session channel authenticates every frame after the handshake, both ways.
//
// The handshake proves both ends hold the link key, but a program in the
// middle (another listener on the port the extension dials, forwarding to the
// real bridge) could pass the genuine handshake through and then inject its
// own frames. So each connection derives two frame keys from the link key and
// that connection's nonces, one per direction:
//
//	key(dir) = HMAC(link key, "oko-frames|" + dir + "|" + origin + "|" + nonceExt + "|" + nonceBridge)
//
// with dir "bridge" for bridge → extension and "ext" for extension → bridge.
// A frame is "<n> <mac> <body>": n counts that direction's frames from 1 with
// no gaps, mac is hex HMAC(key(dir), n + "|" + body). A frame that does not
// check out, or comes out of order, closes the session. A middleman without
// the link key can still drop the connection, but cannot add, change, replay,
// reorder or reflect a frame. extension/protocol.js mirrors this.

var errFrame = errors.New("a frame failed authentication")

type channel struct {
	sendKey, recvKey []byte
	sent, recvd      uint64
}

func frameKey(link []byte, dir, origin, nonceExt, nonceBridge string) []byte {
	return hmacSum(link, strings.Join([]string{"oko-frames", dir, origin, nonceExt, nonceBridge}, "|"))
}

// newChannel returns the channel for one end of a session; bridge says which.
func newChannel(link []byte, origin, nonceExt, nonceBridge string, bridge bool) *channel {
	toExt := frameKey(link, "bridge", origin, nonceExt, nonceBridge)
	toBridge := frameKey(link, "ext", origin, nonceExt, nonceBridge)
	if bridge {
		return &channel{sendKey: toExt, recvKey: toBridge}
	}
	return &channel{sendKey: toBridge, recvKey: toExt}
}

// seal frames body as the next outgoing frame. Callers serialise seal and the
// write so frames reach the wire in counter order.
func (c *channel) seal(body []byte) string {
	c.sent++
	n := strconv.FormatUint(c.sent, 10)
	return n + " " + hmacHex(c.sendKey, n+"|"+string(body)) + " " + string(body)
}

// open checks the next incoming frame and returns its body.
func (c *channel) open(frame string) ([]byte, error) {
	n, rest, ok := strings.Cut(frame, " ")
	if !ok {
		return nil, errFrame
	}
	mac, body, ok := strings.Cut(rest, " ")
	if !ok || n != strconv.FormatUint(c.recvd+1, 10) {
		return nil, errFrame
	}
	got, err := hex.DecodeString(mac)
	if err != nil || !hmac.Equal(got, hmacSum(c.recvKey, n+"|"+body)) {
		return nil, errFrame
	}
	c.recvd++
	return []byte(body), nil
}
