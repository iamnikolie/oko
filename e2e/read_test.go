package e2e

import "testing"

func TestReadArticle(t *testing.T) {
	oko(t, "open", base+"/article.html")
	r := oko(t, "read")
	must(t, r,
		"| main -->",
		"# Agents need a better web",
		"By Ann, Bob and Cy",  // prose split across elements
		"**first**",           // emphasis kept
		"**Site** new | past", // CSS margin separates inline words
		"**AI**-powered",      // no gap, no space
		"[long external link title here](https://example.com/x?id=7)", // tracking param dropped
		"| WebKit | Active | Apple |", "| Blink | Active | Google |",  // rowspan expanded
		"```go", "- one",
		"related cards omitted",
	)
	mustNot(t, r, "Home", "Copyright footer", "Subscribe", "Cookie banner", "[3]", "More 1")
	must(t, oko(t, "read", "--full"), "Copyright footer")
}

func TestReadCards(t *testing.T) {
	oko(t, "open", base+"/cards.html")
	r := oko(t, "read")
	must(t, r,
		"- [Go Engineer](/jobs/1) · Acme · Warsaw · 2 days ago", // role=list of divs + overlay link folded in
		"Orders pending · 1", // stat tile compacted
	)
}
