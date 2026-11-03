// Package bridge lets oko drive the user's own Chrome through the oko browser
// extension. Since Chrome 136 the default profile cannot be started with a
// debugging port, so oko cannot attach to it; the extension can, per tab,
// through chrome.debugger.
//
// A detached "oko _bridge" process listens on 127.0.0.1. The extension
// connects to it (/ext) after a one-time pairing; oko commands connect to it
// (/cdp/<secret>) exactly as they connect to a Chrome they launched. The bridge
// answers the browser-level part of CDP (Target.*: list, attach, create, close)
// itself, from the tabs the user handed over in the extension, and relays
// everything per tab to the extension, which runs it with
// chrome.debugger.sendCommand.
package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultPort is where the bridge listens unless $OKO_EXT_PORT says otherwise.
// The extension popup has the same default.
const DefaultPort = 47222

// Port is the bridge's loopback port.
func Port() int {
	if v := os.Getenv("OKO_EXT_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return DefaultPort
}

// Dir holds the bridge's state: pairing, client secret, pid, log.
func Dir(home string) string { return filepath.Join(home, "extension") }

func secretPath(dir string) string  { return filepath.Join(dir, "client.secret") }
func pairingPath(dir string) string { return filepath.Join(dir, "paired.json") }
func pidPath(dir string) string     { return filepath.Join(dir, "bridge.pid") }

// LogPath is the bridge's log file.
func LogPath(dir string) string { return filepath.Join(dir, "bridge.log") }

// ClientSecret returns the secret oko commands present to the bridge,
// creating it on first use. It sits in a 0600 file: only this user's
// processes can drive the bridge.
func ClientSecret(dir string) (string, error) {
	if b, err := os.ReadFile(secretPath(dir)); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			return s, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	s, err := randomHex(32)
	if err != nil {
		return "", err
	}
	tmp := secretPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, secretPath(dir)); err != nil {
		return "", err
	}
	return s, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("no randomness: " + err.Error())
	}
	return hex.EncodeToString(b), nil
}
