package e2e

import (
	"os"
	"strings"
	"testing"
)

// The picker as the user drives it: Alt+P, click, note, Enter. oko's own
// input is trusted CDP input, the same events a person produces.
func TestPickerReachesAgent(t *testing.T) {
	os.Setenv("OKO_WATCH", "1")
	defer os.Unsetenv("OKO_WATCH")
	defer oko(t, "watch", "stop")

	// The default session: its tab is the visible one (clicks hang in a
	// hidden background tab of headless Chrome).
	oko(t, "open", base+"/pick.html")
	must(t, oko(t, "watch", "start"), "watching")
	snap := oko(t, "snap")
	details := ref(t, snap, `button "Details"`)

	// The page sees neither the picker nor its binding.
	must(t, oko(t, "eval", "typeof window.__okoPicker + ' ' + typeof window.__okoPick"), "undefined undefined")

	oko(t, "press", "Alt+p")
	waitFor(t, func() bool { return strings.Contains(oko(t, "eval", "!!document.querySelector('oko-picker')"), "true") })
	oko(t, "click", details)
	oko(t, "type", "should be secondary")
	oko(t, "press", "Enter")

	// The click went to the picker, not the page.
	must(t, oko(t, "eval", "document.title"), "Pick")

	var out string
	waitFor(t, func() bool {
		out = oko(t, "picks", "--peek")
		return strings.Contains(out, "<oko-picks")
	})
	must(t, out, "["+details+`] button "Details"`, "(in: #1042 Ira K. 249 zł)", `note: "should be secondary"`, "shot ", "css: ")

	// Picks in a tab another session works in are not handed to this one.
	hook := func(sess string) string {
		t.Helper()
		c := cmdWithStdin(`{"session_id":"`+sess+`"}`, "picks", "--hook")
		return c
	}
	if got := hook("someone-else"); got != "" {
		t.Fatalf("another session got the pick:\n%s", got)
	}
	must(t, hook("default"), "The user picked these elements", `note: "should be secondary"`)
	if got := hook("default"); got != "" {
		t.Fatalf("pick delivered twice:\n%s", got)
	}

	// The ref from the pick drives oko directly.
	oko(t, "click", details)
	must(t, oko(t, "eval", "document.title"), "clicked")
}
