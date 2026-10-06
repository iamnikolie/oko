package e2e

import (
	"strings"
	"testing"
)

// as runs oko as the given session; a leading "!" expects failure.
func as(t *testing.T, sess string, args ...string) string {
	t.Helper()
	if len(args) > 0 && args[0] == "!" {
		return oko(t, append([]string{"!", "--session", sess}, args[1:]...)...)
	}
	return oko(t, append([]string{"--session", sess}, args...)...)
}

// tabID is the short id on the first line of 'oko open' output.
func tabID(out string) string {
	return strings.Fields(out)[0]
}

func TestSessionsKeepOwnTabs(t *testing.T) {
	a := tabID(as(t, "sa", "open", base+"/form.html"))
	b := tabID(as(t, "sb", "open", base+"/article.html"))
	if a == b {
		t.Fatalf("both sessions got tab %s", a)
	}
	must(t, as(t, "sa", "eval", "location.pathname"), "/form.html")
	must(t, as(t, "sb", "eval", "location.pathname"), "/article.html")

	// B's new tab does not move A's current tab.
	c := tabID(as(t, "sb", "open", base+"/cards.html", "--new"))
	must(t, as(t, "sa", "eval", "location.pathname"), "/form.html")
	must(t, as(t, "sb", "eval", "location.pathname"), "/cards.html")

	ls := as(t, "sa", "tabs")
	must(t, ls, "* "+a+"  you", "  "+b+"  sb", "  "+c+"  sb")

	// Another session's tab: navigating or closing it is refused.
	must(t, as(t, "sb", "!", "close", a), "belongs to session sa")
	must(t, as(t, "sb", "!", "--tab", a, "open", base+"/feed.html"), "belongs to session sa")
	// Reading it only warns.
	must(t, as(t, "sb", "--tab", a, "eval", "location.pathname"), "belongs to session sa", "/form.html")

	// close without an id closes only B's current tab.
	must(t, as(t, "sb", "close"), "closed "+c)
	ls = as(t, "sa", "tabs")
	must(t, ls, a, b)
	mustNot(t, ls, c)
	must(t, as(t, "sa", "eval", "location.pathname"), "/form.html")

	as(t, "sb", "close", b)
	as(t, "sa", "close")
}

func TestSessionDriftNote(t *testing.T) {
	a := tabID(as(t, "da", "open", base+"/form.html"))
	// Someone else moves A's tab (forced, as a misbehaving caller would).
	as(t, "db", "--tab", a, "open", base+"/article.html", "--force")
	must(t, as(t, "da", "eval", "location.pathname"), "navigated since your last command", "/article.html")
	mustNot(t, as(t, "da", "eval", "1"), "navigated since")
	as(t, "da", "close")
}

// A cold 'open --new' takes Chrome's startup blank tab instead of leaving it
// behind next to a second window.
func TestColdOpenNewTakesStartupTab(t *testing.T) {
	const prof = "coldnew"
	// Remember headless for the profile, then stop it so 'open' starts it.
	oko(t, "--profile", prof, "up", "--headless")
	oko(t, "--profile", prof, "down")
	defer oko(t, "--profile", prof, "down")

	must(t, oko(t, "--profile", prof, "open", base+"/form.html", "--new"), "started chrome", "/form.html")
	ls := oko(t, "--profile", prof, "tabs")
	if n := len(strings.Split(strings.TrimSpace(ls), "\n")); n != 1 {
		t.Fatalf("want one tab after a cold open --new, got %d:\n%s", n, ls)
	}
	must(t, ls, "* ", "  you", "/form.html")
	a := tabID(strings.TrimPrefix(ls, "* "))

	// Once running, --new still opens a separate tab.
	b := tabID(oko(t, "--profile", prof, "open", base+"/article.html", "--new"))
	if a == b {
		t.Fatalf("second --new reused tab %s", a)
	}
}
