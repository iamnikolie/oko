package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// In-page selects. Chrome draws a native <select> popup as an OS widget
// outside the page, so screencast frames and screenshots never show it and a
// recorded fill looks like the value jumping. Chrome's customizable select
// (appearance: base-select) renders the picker in the page's top layer
// instead, with real <option> boxes the pointer can travel to. Recordings and
// --human runs switch selects to it; 'record start --native-selects' and
// 'oko up --native-selects' keep the native control.

// pickerSel matches dropdown selects; multiple and size>1 selects are
// listboxes already drawn in the page.
const pickerSel = `select:not([multiple], [size]:not([size="0"], [size="1"]))`

// selectCSS is wrapped in :where() so the page's own select styles win; only
// the appearance switch is meant to override. --oko-w/--oko-h hold the size
// the control had natively: base-select sizes to the selected option, and an
// empty first option would shrink the field to its arrow.
var selectCSS = strings.ReplaceAll(`
:where(S), :where(S)::picker(select) { appearance: base-select; }
:where(S) { font: inherit; color: inherit; box-sizing: border-box; min-width: var(--oko-w, auto); min-height: var(--oko-h, auto); }
:where(S)[data-oko-arrow]::picker-icon { display: none; }
:where(S)::picker(select) { margin-block: 4px; padding: 4px; border: 1px solid rgba(0,0,0,.14); border-radius: 8px;
  background: Canvas; color: CanvasText; box-shadow: 0 10px 28px rgba(0,0,0,.18), 0 2px 6px rgba(0,0,0,.08);
  max-height: min(320px, 60vh); overflow-y: auto; }
:where(S) option { padding: 6px 10px; border-radius: 5px; }
:where(S) option:hover { background: rgba(37,99,235,.12); }
:where(S) option:checked { background: rgba(37,99,235,.18); }
:where(S) option:disabled { opacity: .45; background: none; }
:where(S) optgroup { font-weight: 600; }
:where(S) optgroup option { font-weight: normal; }
`, "S", pickerSel)

// selectJS is a function of a root (document or open shadow root) that
// installs selectCSS there, records each select's native size, and keeps
// doing so for selects added later. It returns false when Chrome has no
// base-select, so callers fall back to setting the value directly.
var selectJS = func() string {
	css, _ := json.Marshal(selectCSS)
	sel, _ := json.Marshal(pickerSel)
	return `function (root) {
  if (!(window.CSS && CSS.supports('appearance', 'base-select'))) return false;
  if (!window.__okoSelects) {
    const SEL = ` + string(sel) + `, CSS_TEXT = ` + string(css) + `;
    const roots = new WeakSet(), measured = new WeakSet(), sheets = [];
    // Size as the page drew it: our sheets off for one synchronous layout.
    const measure = (r) => {
      const els = Array.from(r.querySelectorAll(SEL)).filter((s) => !measured.has(s));
      if (!els.length) return;
      for (const st of sheets) st.disabled = true;
      const m = els.map((s) => [s, s.offsetWidth, s.offsetHeight, getComputedStyle(s).appearance]);
      for (const st of sheets) st.disabled = false;
      for (const [s, w, h, a] of m) {
        if (!w) continue; // not rendered yet; measured when it shows up
        measured.add(s);
        s.style.setProperty('--oko-w', w + 'px');
        s.style.setProperty('--oko-h', h + 'px');
        if (a === 'none') s.setAttribute('data-oko-arrow', ''); // the page draws its own arrow
      }
    };
    const install = (r) => {
      if (roots.has(r)) { measure(r); return; }
      const host = r === document ? (document.head || document.documentElement) : r;
      if (!host) { addEventListener('DOMContentLoaded', () => install(r), { once: true }); return; }
      roots.add(r);
      const st = document.createElement('style');
      st.id = '__oko_select';
      st.textContent = CSS_TEXT;
      host.appendChild(st);
      sheets.push(st);
      measure(r);
      let queued = false;
      new MutationObserver(() => {
        if (queued) return;
        queued = true;
        setTimeout(() => { queued = false; measure(r); }, 30);
      }).observe(r, { childList: true, subtree: true });
      if (r === document) {
        for (const el of document.querySelectorAll('*')) if (el.shadowRoot) install(el.shadowRoot);
      }
    };
    window.__okoSelects = install;
  }
  window.__okoSelects(document);
  if (root && root !== document) window.__okoSelects(root);
  return true;
}`
}()

// selectDocJS installs in-page selects in the current document.
var selectDocJS = "(" + selectJS + ")(document)"

// recordingOf reports whether the tab has a live recorder, and whether that
// recording keeps native selects.
func (s *session) recordingOf(p *rod.Page) (rec, native bool) {
	pid := s.prof.State.Recorders[string(p.TargetID)]
	if pid == 0 || syscall.Kill(pid, 0) != nil {
		return false, false
	}
	_, err := os.Stat(filepath.Join(recordDir(s.prof, p.TargetID), "native-selects"))
	return true, err == nil
}

// inPageSelects reports whether selects should open in the page: while the
// tab is recording, or under --human so a rehearsal looks like the take.
func (s *session) inPageSelects(p *rod.Page) bool {
	if s.prof.State.NativeSelects {
		return false
	}
	rec, native := s.recordingOf(p)
	if native {
		return false
	}
	return rec || s.human()
}

const optionIndexJS = `function (v) {
  const o = Array.from(this.options);
  let i = o.findIndex((x) => x.innerText.includes(v));
  if (i < 0) i = o.findIndex((x) => x.value === v);
  if (i < 0) return -1;
  if (o[i].disabled || (o[i].parentElement.tagName === 'OPTGROUP' && o[i].parentElement.disabled)) return -2;
  return i;
}`

// pickOption opens the select's in-page picker, moves the pointer to the
// option and clicks it. When the picker cannot be used here (no base-select,
// the page forces another appearance) it returns only a reason, and the
// caller sets the value directly; for a listbox select, already drawn in the
// page, it returns nothing at all.
func (s *session) pickOption(p *rod.Page, el *rod.Element, value, d string) (msg, reason string, err error) {
	r, err := el.Eval(`function () {
  if (this.multiple || this.size > 1) return 'listbox';
  return (` + selectJS + `)(this.getRootNode()) ? getComputedStyle(this).appearance : 'unsupported';
}`)
	if err != nil {
		return "", "", err
	}
	switch a := r.Value.Str(); a {
	case "base-select":
	case "listbox":
		return "", "", nil // drawn in the page already
	case "unsupported":
		return "", "this Chrome has no in-page select (appearance: base-select)", nil
	default:
		return "", fmt.Sprintf("select keeps appearance %q (page CSS overrides it)", a), nil
	}
	r, err = el.Eval(optionIndexJS, value)
	if err != nil {
		return "", "", err
	}
	idx := r.Value.Int()
	switch idx {
	case -1:
		return "", "", fmt.Errorf("no option %q in %s", value, d)
	case -2:
		return "", "", fmt.Errorf("option %q in %s is disabled", value, d)
	}
	if _, err := el.Interactable(); err != nil {
		if _, err := interactable(el); err != nil {
			return "", "", err
		}
	}
	isOpen := func() bool {
		r, err := el.Eval(`function () { return this.matches(':open') }`)
		return err == nil && r.Value.Bool()
	}
	waitFor := func(cond func() bool) bool {
		for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return false
	}
	if err := s.humanClick(p, el, proto.InputMouseButtonLeft, 1); err != nil {
		return "", "", err
	}
	if !waitFor(isOpen) {
		return "", "the picker did not open", nil
	}
	pause(250*time.Millisecond, 500*time.Millisecond) // let the open list be seen
	opt, err := el.ElementByJS(rod.Eval(`function (i) { const o = this.options[i]; o.scrollIntoView({ block: 'nearest' }); return o }`, idx))
	if err != nil {
		return "", "", err
	}
	if off, ok := frameOffsets[el]; ok {
		frameOffsets[opt] = off
	}
	want, err := opt.Eval(`function () { return this.value }`)
	if err != nil {
		return "", "", err
	}
	if err := s.humanClick(p, opt, proto.InputMouseButtonLeft, 1); err != nil {
		return "", "", err
	}
	picked := waitFor(func() bool {
		r, err := el.Eval(`function (v) { return !this.matches(':open') && this.value === v }`, want.Value.Str())
		return err == nil && r.Value.Bool()
	})
	if !picked {
		if isOpen() {
			_ = p.KeyActions().Type(input.Escape).Do()
		}
		return "", "the picker did not take the click", nil
	}
	return fmt.Sprintf("selected %q in %s (in-page picker)", value, d), "", nil
}
