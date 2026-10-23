package e2e

import (
	"path/filepath"
	"testing"
)

// A recording opens selects in the page (appearance: base-select), so the
// picker and the pointer's way to the option end up in the video.
func TestSelectOpensInRecording(t *testing.T) {
	if oko(t, "eval", "CSS.supports('appearance', 'base-select')") != "true\n" {
		t.Skip("Chrome without customizable select")
	}
	oko(t, "open", base+"/select.html")
	oko(t, "record", "start")
	out := oko(t, "fill", "#s", "Cherry")
	oko(t, "record", "stop", "-o", filepath.Join(t.TempDir(), "s.gif"))
	must(t, out, `selected "Cherry"`, "(in-page picker)", `combobox "Fruit" = "Cherry"`)
	must(t, oko(t, "eval", "[wasOpen, optionShown, changes, s.value].join(' ')"), "true true 1 c")
	// The closed field keeps its native width instead of shrinking to the
	// empty first option.
	must(t, oko(t, "eval", "s.offsetWidth > 60"), "true")

	// --native-selects: no style, the value is set directly.
	oko(t, "open", base+"/select.html")
	oko(t, "record", "start", "--native-selects")
	must(t, oko(t, "eval", "!!document.getElementById('__oko_select')"), "false")
	out = oko(t, "fill", "#s", "Apple")
	oko(t, "record", "stop", "-o", filepath.Join(t.TempDir(), "s.gif"))
	mustNot(t, out, "in-page picker")
	must(t, oko(t, "eval", "[wasOpen, changes, s.value].join(' ')"), "false 1 a")
}

func TestSelectHumanAndPlain(t *testing.T) {
	if oko(t, "eval", "CSS.supports('appearance', 'base-select')") != "true\n" {
		t.Skip("Chrome without customizable select")
	}
	oko(t, "open", base+"/select.html")
	must(t, oko(t, "--human", "fill", "#s", "Banana"), "(in-page picker)")
	must(t, oko(t, "eval", "[wasOpen, changes, s.value].join(' ')"), "true 1 b")
	// Disabled options cannot be picked.
	oko(t, "eval", "s.options[4].disabled = true")
	must(t, oko(t, "!", "--human", "fill", "#s", "Durian"), "is disabled")

	// Without --human or a recording: the fast direct path, no style.
	oko(t, "open", base+"/select.html")
	out := oko(t, "fill", "#s", "Durian")
	mustNot(t, out, "in-page picker")
	must(t, oko(t, "eval", "[wasOpen, changes, s.value, !!document.getElementById('__oko_select')].join(' ')"), "false 1 d false")
}
