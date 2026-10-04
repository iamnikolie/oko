package e2e

import "testing"

func TestChangesAndSignals(t *testing.T) {
	oko(t, "open", base+"/signals.html")
	s := oko(t, "snap")

	// invalid field: signal with the error text
	out := oko(t, "click", ref(t, s, `button "Save"`))
	must(t, out, `invalid [`, `"Email": "Enter a valid email"`)

	// a toast that disappears after 300 ms is still reported
	oko(t, "fill", ref(t, s, `textbox "Email"`), "me@x.io")
	out = oko(t, "click", ref(t, s, `button "Save"`))
	must(t, out, `message "Saved!"`)

	// expand: the button changes state and a link appears
	out = oko(t, "click", ref(t, s, `button "Details"`))
	must(t, out, "changes:", `~ [`, `button "Details" expanded`, `(was: button "Details" collapsed)`, `+ [`, `link "Extra link"`)

	// dialog opens and closes
	out = oko(t, "click", ref(t, s, `button "New product"`))
	must(t, out, `dialog "New product" opened:`, `textbox "Name"`)
	out = oko(t, "click", "text=Close")
	must(t, out, `dialog "New product" closed`)

	// nothing happens
	out = oko(t, "hover", "h1")
	must(t, out, "no visible change")
}
