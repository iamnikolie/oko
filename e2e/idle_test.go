package e2e

import (
	"strings"
	"testing"
	"time"
)

func TestNoWebdriverFlag(t *testing.T) {
	oko(t, "open", base+"/form.html")
	must(t, oko(t, "eval", "String(navigator.webdriver)"), "false")
}

// An idle browser closes by itself and the next command starts it again.
func TestIdleClose(t *testing.T) {
	p := []string{"-p", "idle"}
	defer oko(t, append(p, "down")...)
	must(t, oko(t, append(p, "up", "--headless", "--idle", "2s")...), "closes after 2s idle")
	// Commands keep it alive past the limit.
	for i := 0; i < 3; i++ {
		time.Sleep(time.Second)
		oko(t, append(p, "eval", "1")...)
	}
	must(t, oko(t, append(p, "status")...), "running")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(oko(t, append(p, "status")...), "stopped") {
		if time.Now().After(deadline) {
			t.Fatal("idle browser still running")
		}
		time.Sleep(300 * time.Millisecond)
	}
	must(t, oko(t, append(p, "eval", "1")...), "1")
	oko(t, append(p, "up", "--idle", "off")...)
	must(t, oko(t, append(p, "status")...), "running", "never closes when idle")
}
