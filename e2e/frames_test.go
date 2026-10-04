package e2e

import "testing"

func TestCrossOriginFrame(t *testing.T) {
	oko(t, "open", base+"/frames.html?"+alt) // child served from a different site
	oko(t, "wait", "iframe")
	s := oko(t, "snap")
	must(t, s, `-- frame f1 "Payment" (`+alt+`) --`, `## Card details`)
	card := ref(t, s, `textbox "Card number"`)
	if card[:2] != "f1" {
		t.Fatalf("frame ref should be prefixed: %s", card)
	}
	oko(t, "fill", card, "4242")
	oko(t, "click", ref(t, s, `button "Pay"`))
	must(t, oko(t, "snap", "--text"), "paid 4242")

	// human mode moves the real mouse into the frame
	oko(t, "fill", card, "1111")
	oko(t, "--human", "click", ref(t, s, `button "Pay"`))
	must(t, oko(t, "snap", "--text"), "paid 1111")

	r := oko(t, "read")
	must(t, r, "Total: $42", "## Frame f1: Payment", "Card details", "paid 1111")
}
