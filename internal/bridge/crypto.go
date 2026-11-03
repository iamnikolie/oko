package bridge

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// The extension link is authenticated both ways and no long-term secret
// ever crosses the socket.
//
// Pairing (once): the extension and the bridge run an ephemeral P-256 ECDH
// and each proves it knows the one-time code with an HMAC over the transcript
// (both public keys, both nonces, the extension origin), keyed with
// PBKDF2(code). The bridge proves first, so a listener that does not hold the
// code learns nothing; PBKDF2 makes an offline guess of the code from a
// captured bridge proof far slower than the code's 10-minute life. The link
// key is HMAC(ECDH secret, transcript): the code never leaves either side and
// the key never travels.
//
// Every connection: HMAC challenge-response over nonces from both sides with
// the link key; again the bridge proves first. The extension sends nothing
// else and runs no command before the bridge's proof checks out. Every frame
// after the handshake is authenticated with keys derived from the link key
// and that connection's nonces (channel.go).

const (
	pairProto      = "oko-pair-v2"
	pairIterations = 100_000
	nonceHexLen    = 32 // 16 random bytes
)

var hexNonceRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validNonce(s string) bool { return hexNonceRe.MatchString(s) }

func hmacSum(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func hmacHex(key []byte, msg string) string { return hex.EncodeToString(hmacSum(key, msg)) }

// hmacOK compares a hex proof against the expected HMAC in constant time.
func hmacOK(key []byte, msg, proof string) bool {
	got, err := hex.DecodeString(proof)
	return err == nil && hmac.Equal(got, hmacSum(key, msg))
}

// codeKey stretches the one-time code; salt is the bridge's nonce.
func codeKey(code, salt string) []byte {
	k, err := pbkdf2.Key(sha256.New, normCode(code), []byte(salt), pairIterations, 32)
	if err != nil {
		return nil
	}
	return k
}

func pairTranscript(origin, pubExt, pubBridge, nonceExt, nonceBridge string) string {
	return strings.Join([]string{pairProto, origin, pubExt, pubBridge, nonceExt, nonceBridge}, "|")
}

func helloBridgeMsg(origin, nonceExt, nonceBridge string) string {
	return "oko-bridge|" + origin + "|" + nonceExt + "|" + nonceBridge
}

func helloExtMsg(origin, nonceExt, nonceBridge string) string {
	return "oko-ext|" + origin + "|" + nonceExt + "|" + nonceBridge
}

func cliBridgeMsg(nonceCLI, nonceBridge string) string {
	return "oko-bridge-cli|" + nonceCLI + "|" + nonceBridge
}

func cliClientMsg(nonceCLI, nonceBridge string) string {
	return "oko-cli|" + nonceCLI + "|" + nonceBridge
}
