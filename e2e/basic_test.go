package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestSnapFillClick(t *testing.T) {
	oko(t, "open", base+"/form.html")
	s := oko(t, "snap")
	must(t, s, `textbox "Email"`, `combobox = "Free"`, `checkbox "I agree" unchecked`, `(in: Invoice 2)`)

	oko(t, "fill", ref(t, s, `textbox "Email"`), "me@x.io")
	oko(t, "fill", ref(t, s, "combobox"), "Pro")
	oko(t, "fill", ref(t, s, `checkbox "I agree"`), "true")
	oko(t, "fill", ref(t, s, `textbox "Notes"`), "hello")
	oko(t, "click", ref(t, s, `button "Send"`))
	must(t, oko(t, "text", "#out"), "sent me@x.io p true")

	// the second Delete is addressed through its row
	var del string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, `button "Delete"`) && strings.Contains(l, "Invoice 2") {
			del = l[1:strings.Index(l, "]")]
		}
	}
	oko(t, "click", del)
	must(t, oko(t, "eval", "[...document.querySelectorAll('td button')].map(b=>b.textContent).join(',')"), "Delete,gone2")
}

func TestStaleRefAfterReload(t *testing.T) {
	oko(t, "open", base+"/form.html")
	s := oko(t, "snap")
	r := ref(t, s, `button "Send"`)
	oko(t, "reload")
	must(t, oko(t, "!", "click", r), "not on the page any more")
	s2 := oko(t, "snap")
	if ref(t, s2, `button "Send"`) == r {
		t.Fatal("ref number reused after reload")
	}
}

func TestDialogAutoAnswer(t *testing.T) {
	oko(t, "open", base+"/form.html")
	must(t, oko(t, "click", "text=Confirm me"), `dialog confirm "sure?" → accepted`)
	must(t, oko(t, "--dialog", "dismiss", "click", "text=confirmed"), "dismissed")
	must(t, oko(t, "text", "body"), "declined")
}

func TestNewTabFollowed(t *testing.T) {
	oko(t, "open", base+"/form.html")
	out := oko(t, "click", "text=Blank")
	must(t, out, "new tab", "Article")
	oko(t, "close")
}

func TestConsoleHistoryAndNet(t *testing.T) {
	oko(t, "open", base+"/form.html")
	c := oko(t, "console")
	must(t, c, `boot {"a":1}`, "careful", "Error: boom")
	must(t, oko(t, "net", "--failed"), "404", "/missing.json")
}

func TestEvalForms(t *testing.T) {
	oko(t, "open", base+"/form.html")
	must(t, oko(t, "eval", "document.title"), "Form")
	must(t, oko(t, "eval", "(()=>{return [1,'<b>']})()"), `"<b>"`)
	must(t, oko(t, "eval", "async () => 6*7"), "42")
	must(t, oko(t, "eval", "el => el.placeholder", "--on", "#email"), "you@x.com")
	must(t, oko(t, "eval", "document.querySelector('h1')"), "<h1>Orders</h1>")
}

func TestShot(t *testing.T) {
	oko(t, "open", base+"/form.html")
	p := strings.TrimSpace(oko(t, "shot", "h1"))
	st, err := os.Stat(p)
	if err != nil || st.Size() < 500 {
		t.Fatalf("bad screenshot %s: %v", p, err)
	}
}

func TestViewportMobile(t *testing.T) {
	oko(t, "open", base+"/form.html")
	must(t, oko(t, "viewport", "390x844", "--mobile"), "390x844")
	must(t, oko(t, "eval", "screen.width + 'x' + screen.height"), "390x844")
	oko(t, "viewport", "reset")
}

// A hidden tab never runs animation frames; clicks must not wait for one.
func TestClickInHiddenTab(t *testing.T) {
	oko(t, "open", base+"/form.html")              // the default session's tab stays visible
	as(t, "hidden-tab", "open", base+"/pick.html") // a fresh tab behind it
	must(t, as(t, "hidden-tab", "eval", "document.visibilityState"), "hidden")
	as(t, "hidden-tab", "--timeout", "8s", "click", "text=Details")
	must(t, as(t, "hidden-tab", "eval", "document.title"), "clicked")
	as(t, "hidden-tab", "--timeout", "8s", "hover", "text=Refund")
	as(t, "hidden-tab", "close")
}
