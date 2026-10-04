package e2e

import (
	"strings"
	"testing"
)

func TestTrimAndRevive(t *testing.T) {
	oko(t, "open", base+"/feed.html")
	oko(t, "eval", "window.scrollTo(0, document.scrollingElement.scrollHeight * 0.8)")
	topBefore := oko(t, "eval", "(() => { const r = [...document.querySelectorAll('li')].find(li => li.getBoundingClientRect().top >= 0); return r.querySelector('h3').textContent })()")
	out := oko(t, "trim", "--keep", "30")
	must(t, out, "removed", "of 400 items in ul#feed", "DOM nodes")
	if strings.Contains(out, "removed 0 ") {
		t.Fatalf("nothing trimmed: %s", out)
	}
	topAfter := oko(t, "eval", "(() => { const r = [...document.querySelectorAll('li')].find(li => li.getBoundingClientRect().top >= 0); return r.querySelector('h3').textContent })()")
	if topBefore != topAfter {
		t.Fatalf("visible position moved: %q → %q", topBefore, topAfter)
	}
	must(t, oko(t, "revive"), "revived", "/feed.html")
	must(t, oko(t, "eval", "document.querySelectorAll('li').length"), "400")
}
