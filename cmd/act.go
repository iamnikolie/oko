package cmd

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

var (
	actSnap bool
	noDiff  bool
)

const maxDiffLines = 25

// reportChanges prints signals and what changed since the baseline taken
// before the action. Without a comparable baseline (navigation, or a dialog
// opening/closing) it says what is on screen now instead.
func reportChanges(p *rod.Page, navigated bool) {
	r, err := takeSnapshot(p, snapOpts{}, true)
	if err != nil {
		return
	}
	if l := r.Signals.line(); l != "" {
		fmt.Fprintln(stdout, "signals: "+l)
	}
	switch {
	case navigated || r.Prev == nil:
		fmt.Fprintf(stdout, "new page: %d elements ('oko snap' to see them)\n", countEls(r.Items))
	case r.Prev.Scope != r.Scope && r.Scope != "page":
		fmt.Fprintf(stdout, "%s opened:\n", r.Scope)
		printLimited(itemLines(r.Items))
	case r.Prev.Scope != r.Scope:
		fmt.Fprintf(stdout, "%s closed; page has %d elements ('oko snap' to see them)\n", r.Prev.Scope, countEls(r.Items))
	default:
		lines := diffLines(r.Prev.Items, r.Items)
		if len(lines) == 0 {
			fmt.Fprintln(stdout, "no visible change")
			return
		}
		fmt.Fprintln(stdout, "changes:")
		printLimited(lines)
	}
}

func itemLines(items []snapItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, renderItem(it))
	}
	return out
}

func printLimited(lines []string) {
	for i, l := range lines {
		if i == maxDiffLines {
			fmt.Fprintf(stdout, "… %d more ('oko snap' for everything)\n", len(lines)-i)
			return
		}
		fmt.Fprintln(stdout, l)
	}
}

func countEls(items []snapItem) int {
	n := 0
	for _, it := range items {
		if it.Kind == "el" {
			n++
		}
	}
	return n
}

const describeJS = `function () {
  const t = this.tagName ? this.tagName.toLowerCase() : 'node';
  const id = this.id ? '#' + this.id : '';
  const txt = (this.innerText || this.value || this.getAttribute && (this.getAttribute('aria-label') || this.getAttribute('placeholder')) || '')
    .replace(/\s+/g, ' ').trim();
  return t + id + (txt ? ' "' + (txt.length > 50 ? txt.slice(0, 49) + '…' : txt) + '"' : '');
}`

func describe(el *rod.Element) string {
	res, err := el.Eval(describeJS)
	if err != nil {
		return "element"
	}
	return res.Value.Str()
}

// act runs an action on the current tab and reports what changed: a new
// tab or a navigation (dialogs are reported by the session). With --snap it then prints a
// snapshot of the resulting page.
func act(fn func(s *session, p *rod.Page) (string, error)) error {
	return run(func(s *session) error {
		p, err := s.page()
		if err != nil {
			return err
		}
		before, _ := p.Info()
		tabsBefore, _ := s.tabs()
		// Baseline for the change report, and a watcher for messages that
		// flash and vanish before the action settles.
		if !actSnap && !noDiff {
			_, _ = takeSnapshot(p, snapOpts{}, false)
			_, _ = p.Eval(chrome.LiveWatchJS)
		}

		msg, err := fn(s, p)
		if err != nil {
			return err
		}
		settle(p)
		fmt.Fprintln(stdout, msg)

		// A click on target=_blank opens a tab; follow it.
		if tabsAfter, err := s.tabs(); err == nil && len(tabsAfter) > len(tabsBefore) {
			known := map[proto.TargetTargetID]bool{}
			for _, t := range tabsBefore {
				known[t.TargetID] = true
			}
			for _, t := range tabsAfter {
				if !known[t.TargetID] {
					s.claim(t.TargetID)
					s.setCurrent(t.TargetID)
					np, err := s.b.PageFromTarget(t.TargetID)
					if err == nil {
						_ = np.Timeout(10 * time.Second).WaitLoad()
						settle(np)
						info, _ := np.Info()
						if info != nil {
							fmt.Fprintf(stdout, "new tab %s (now current): %s\n%s\n", shortID(t.TargetID), info.Title, info.URL)
						}
						if actSnap {
							fmt.Fprintln(stdout)
							return snapshot(np, snapOpts{max: snapMax})
						}
					}
					return nil
				}
			}
		}
		navigated := false
		if after, err := p.Info(); err == nil && before != nil && after.URL != before.URL {
			fmt.Fprintf(stdout, "→ %s\n  %s\n", after.URL, after.Title)
			navigated = true
		}
		if actSnap {
			fmt.Fprintln(stdout)
			return snapshot(p, snapOpts{max: snapMax})
		}
		if !noDiff {
			reportChanges(p, navigated)
		}
		return nil
	})
}

// interactable scrolls el into view and waits briefly until a pointer could
// hit it, explaining what is in the way when it cannot.
// scrollIntoView scrolls the element into view without waiting for an
// animation frame. rod's ScrollIntoView (and Click/Hover/Focus, which call it)
// first waits for requestAnimationFrame, which never fires in a hidden tab:
// a background tab of headless Chrome, a minimized window.
func scrollIntoView(el *rod.Element) {
	_ = proto.DOMScrollIntoViewIfNeeded{ObjectID: el.Object.ObjectID}.Call(el)
}

// interactable scrolls the element into view and waits (bounded) until a
// click would land on it; it returns the point to click.
func interactable(el *rod.Element) (*proto.Point, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		scrollIntoView(el)
		pt, err := el.Interactable()
		if err == nil {
			return pt, nil
		}
		if time.Now().After(deadline) {
			var cov *rod.CoveredError
			if errors.As(err, &cov) {
				return nil, fmt.Errorf("element is covered by %s (close the overlay, or use --js to click through)", describe(cov.Element))
			}
			return nil, fmt.Errorf("element is not clickable: %v (use --js to click through)", err)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// clickAt moves the mouse to the element's point and clicks there, like
// rod's Element.Click without its animation-frame waits.
func clickAt(el *rod.Element, pt *proto.Point, btn proto.InputMouseButton, n int) error {
	if err := el.WaitEnabled(); err != nil {
		return err
	}
	m := el.Page().Mouse
	if err := m.MoveTo(*pt); err != nil {
		return err
	}
	return m.Click(btn, n)
}

var (
	clickJS     bool
	clickDouble bool
	clickRight  bool
)

var clickCmd = &cobra.Command{
	Use:   "click <target>",
	Short: "Click an element (ref, text=…, or CSS selector)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return act(func(s *session, p *rod.Page) (string, error) {
			el, err := element(p, args[0])
			if err != nil {
				return "", err
			}
			d := describe(el)
			if clickJS {
				if _, err := el.Eval(`function () { this.click() }`); err != nil {
					return "", err
				}
				return "clicked (js) " + d, nil
			}
			btn, n := proto.InputMouseButtonLeft, 1
			if clickRight {
				btn = proto.InputMouseButtonRight
			}
			if clickDouble {
				n = 2
			}
			if s.human() {
				if _, err := el.Interactable(); err != nil {
					if _, err := interactable(el); err != nil {
						return "", err
					}
				}
				if err := s.humanClick(p, el, btn, n); err != nil {
					return "", err
				}
				return "clicked " + d + " (human)", nil
			}
			pt, err := interactable(el)
			if err != nil {
				return "", err
			}
			if err := clickAt(el, pt, btn, n); err != nil {
				return "", err
			}
			return "clicked " + d, nil
		})
	},
}

var hoverCmd = &cobra.Command{
	Use:   "hover <target>",
	Short: "Move the mouse over an element",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return act(func(s *session, p *rod.Page) (string, error) {
			el, err := element(p, args[0])
			if err != nil {
				return "", err
			}
			if s.human() {
				if err := s.humanHover(p, el); err != nil {
					return "", err
				}
				return "hovered " + describe(el) + " (human)", nil
			}
			pt, err := interactable(el)
			if err != nil {
				return "", err
			}
			if err := el.Page().Mouse.MoveTo(*pt); err != nil {
				return "", err
			}
			return "hovered " + describe(el), nil
		})
	},
}

const fieldKindJS = `function () {
  if (this.tagName === 'SELECT') return 'select';
  if (this.tagName === 'TEXTAREA') return 'text';
  if (this.tagName === 'INPUT') {
    const t = (this.type || 'text').toLowerCase();
    if (t === 'checkbox' || t === 'radio') return 'check';
    if (t === 'file') return 'file';
    if (['date', 'time', 'month', 'week', 'datetime-local', 'color', 'range'].includes(t)) return 'native';
    return 'text';
  }
  const r = this.getAttribute('role');
  if (r === 'checkbox' || r === 'switch' || r === 'radio') return 'aria-check';
  if (this.isContentEditable) return 'editable';
  return 'text';
}`

// setNativeJS sets a value the way a framework expects it: through the
// prototype setter, then input + change events.
const setNativeJS = `function (v) {
  const proto = this.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype :
    this.tagName === 'SELECT' ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, 'value').set.call(this, v);
  this.dispatchEvent(new Event('input', { bubbles: true }));
  this.dispatchEvent(new Event('change', { bubbles: true }));
  return this.value;
}`

const selectAllJS = `function () {
  this.focus();
  if (typeof this.select === 'function') { this.select(); return; }
  const r = document.createRange();
  r.selectNodeContents(this);
  const s = window.getSelection();
  s.removeAllRanges();
  s.addRange(r);
}`

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on", "checked", "y":
		return true, nil
	case "0", "false", "no", "off", "unchecked", "n":
		return false, nil
	}
	return false, fmt.Errorf("checkbox value must be true/false, got %q", v)
}

func fill(s *session, p *rod.Page, el *rod.Element, value string) (string, error) {
	res, err := el.Eval(fieldKindJS)
	if err != nil {
		return "", err
	}
	d := describe(el)
	switch kind := res.Value.Str(); kind {
	case "select":
		if err := el.Select([]string{value}, true, rod.SelectorTypeText); err != nil {
			// Not an option label; try it as an option value.
			r, err2 := el.Eval(`function (v) { const o = Array.from(this.options).find(o => o.value === v); if (!o) return false; this.value = v; this.dispatchEvent(new Event('input', {bubbles: true})); this.dispatchEvent(new Event('change', {bubbles: true})); return true }`, value)
			if err2 != nil || !r.Value.Bool() {
				return "", fmt.Errorf("no option %q in %s", value, d)
			}
		}
		return fmt.Sprintf("selected %q in %s", value, d), nil
	case "check", "aria-check":
		want, err := parseBool(value)
		if err != nil {
			return "", err
		}
		r, err := el.Eval(`function () { return this.tagName === 'INPUT' ? this.checked : this.getAttribute('aria-checked') === 'true' }`)
		if err != nil {
			return "", err
		}
		if r.Value.Bool() != want {
			if _, err := el.Eval(`function () { this.click() }`); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("set %s to %v", d, want), nil
	case "file":
		return "", errors.New("file input: use 'oko upload <target> <file>'")
	case "native":
		if _, err := el.Eval(setNativeJS, value); err != nil {
			return "", err
		}
		return fmt.Sprintf("filled %s = %q", d, value), nil
	default:
		// Still fillable when not clickable: focus does not need a pointer.
		_, _ = interactable(el)
		if s.human() {
			if err := s.humanClick(p, el, proto.InputMouseButtonLeft, 1); err != nil {
				return "", err
			}
			pause(100*time.Millisecond, 300*time.Millisecond)
		}
		if _, err := el.Eval(selectAllJS); err != nil {
			return "", err
		}
		if value != "" && s.human() {
			if err := humanType(p, value); err != nil {
				return "", err
			}
		} else if value == "" {
			if err := p.KeyActions().Type(input.Backspace).Do(); err != nil {
				return "", err
			}
		} else if err := p.InsertText(value); err != nil {
			return "", err
		}
		return fmt.Sprintf("filled %s = %q", d, trunc(value, 60)), nil
	}
}

var fillCmd = &cobra.Command{
	Use:   "fill <target> <value>",
	Short: "Replace an input's value; also selects options and sets checkboxes (true/false)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return act(func(s *session, p *rod.Page) (string, error) {
			el, err := element(p, args[0])
			if err != nil {
				return "", err
			}
			return fill(s, p, el, args[1])
		})
	},
}

var (
	typeInto   string
	typeSubmit bool
)

var typeCmd = &cobra.Command{
	Use:   "type <text>",
	Short: "Type text at the cursor (focused element), without clearing",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return act(func(s *session, p *rod.Page) (string, error) {
			if typeInto != "" {
				el, err := element(p, typeInto)
				if err != nil {
					return "", err
				}
				scrollIntoView(el)
				if err := (proto.DOMFocus{ObjectID: el.Object.ObjectID}).Call(el); err != nil {
					return "", err
				}
			}
			if s.human() {
				if err := humanType(p, args[0]); err != nil {
					return "", err
				}
			} else if err := p.InsertText(args[0]); err != nil {
				return "", err
			}
			msg := fmt.Sprintf("typed %q", trunc(args[0], 60))
			if typeSubmit {
				if err := p.KeyActions().Type(input.Enter).Do(); err != nil {
					return "", err
				}
				msg += " + Enter"
			}
			return msg, nil
		})
	},
}

var keyNames = map[string]input.Key{
	"enter": input.Enter, "return": input.Enter, "tab": input.Tab, "escape": input.Escape, "esc": input.Escape,
	"backspace": input.Backspace, "delete": input.Delete, "del": input.Delete, "space": input.Space,
	"arrowup": input.ArrowUp, "up": input.ArrowUp, "arrowdown": input.ArrowDown, "down": input.ArrowDown,
	"arrowleft": input.ArrowLeft, "left": input.ArrowLeft, "arrowright": input.ArrowRight, "right": input.ArrowRight,
	"home": input.Home, "end": input.End, "pageup": input.PageUp, "pagedown": input.PageDown, "insert": input.Insert,
	"shift": input.ShiftLeft, "control": input.ControlLeft, "ctrl": input.ControlLeft,
	"alt": input.AltLeft, "option": input.AltLeft, "meta": input.MetaLeft, "cmd": input.MetaLeft, "command": input.MetaLeft,
	"f1": input.F1, "f2": input.F2, "f3": input.F3, "f4": input.F4, "f5": input.F5, "f6": input.F6,
	"f7": input.F7, "f8": input.F8, "f9": input.F9, "f10": input.F10, "f11": input.F11, "f12": input.F12,
}

func parseKey(name string) (input.Key, error) {
	if k, ok := keyNames[strings.ToLower(name)]; ok {
		return k, nil
	}
	r := []rune(name)
	if len(r) == 1 {
		k := input.Key(r[0])
		if r[0] >= 'A' && r[0] <= 'Z' {
			k = input.Key(r[0] + ('a' - 'A'))
		}
		if _, err := safeInfo(k); err == nil {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown key %q", name)
}

// safeInfo guards Key.Info, which panics on keys missing from rod's keymap.
func safeInfo(k input.Key) (info input.KeyInfo, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("no such key")
		}
	}()
	return k.Info(), nil
}

var pressCmd = &cobra.Command{
	Use:   "press <keys>...",
	Short: "Press keys or combos in order: Enter, Tab, Escape, Control+a, Shift+Tab, Meta+Enter",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		type combo struct{ mods, keys []input.Key }
		var combos []combo
		for _, a := range args {
			parts := strings.Split(a, "+")
			if strings.HasSuffix(a, "++") { // "Control++"
				parts = append(strings.Split(strings.TrimSuffix(a, "++"), "+"), "+")
			}
			var c combo
			for i, part := range parts {
				k, err := parseKey(part)
				if err != nil {
					return err
				}
				if i < len(parts)-1 {
					c.mods = append(c.mods, k)
				} else {
					c.keys = append(c.keys, k)
				}
			}
			combos = append(combos, c)
		}
		return act(func(s *session, p *rod.Page) (string, error) {
			for _, c := range combos {
				ka := p.KeyActions()
				if len(c.mods) > 0 {
					ka = ka.Press(c.mods...)
				}
				ka = ka.Type(c.keys...)
				if len(c.mods) > 0 {
					ka = ka.Release(c.mods...)
				}
				if err := ka.Do(); err != nil {
					return "", err
				}
			}
			return "pressed " + strings.Join(args, " "), nil
		})
	},
}

var scrollCmd = &cobra.Command{
	Use:   "scroll [down|up|top|bottom|<target>] [px]",
	Short: "Scroll the page (default: one screen down) or scroll an element into view",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		where := "down"
		if len(args) > 0 {
			where = args[0]
		}
		px := 0
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("px must be a number")
			}
			px = n
		}
		return act(func(s *session, p *rod.Page) (string, error) {
			if s.human() && (where == "down" || where == "up" || where == "top" || where == "bottom") {
				return humanScrollCmd(s, p, where, px)
			}
			var js string
			switch where {
			case "down":
				js = fmt.Sprintf(`() => { window.scrollBy(0, %d || Math.round(innerHeight * 0.85)); }`, px)
			case "up":
				js = fmt.Sprintf(`() => { window.scrollBy(0, -(%d || Math.round(innerHeight * 0.85))); }`, px)
			case "top":
				js = `() => { window.scrollTo(0, 0); }`
			case "bottom":
				js = `() => { window.scrollTo(0, document.scrollingElement.scrollHeight); }`
			default:
				el, err := element(p, where)
				if err != nil {
					return "", err
				}
				if s.human() {
					if err := s.humanScrollTo(p, el); err != nil {
						return "", err
					}
				} else if _, err := el.Eval(`function () { this.scrollIntoView({block: 'center'}) }`); err != nil {
					return "", err
				}
				return "scrolled to " + describe(el), nil
			}
			if _, err := p.Eval(js); err != nil {
				return "", err
			}
			res, err := p.Eval(`() => Math.round(scrollY) + '/' + document.scrollingElement.scrollHeight`)
			if err != nil {
				return "", err
			}
			return "scroll " + res.Value.Str(), nil
		})
	},
}

var uploadCmd = &cobra.Command{
	Use:   "upload <target> <file>...",
	Short: "Set files on a file input",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var files []string
		for _, f := range args[1:] {
			abs, err := filepath.Abs(f)
			if err != nil {
				return err
			}
			files = append(files, abs)
		}
		return act(func(s *session, p *rod.Page) (string, error) {
			el, err := element(p, args[0])
			if err != nil {
				return "", err
			}
			if err := el.SetFiles(files); err != nil {
				return "", err
			}
			return fmt.Sprintf("uploaded %d file(s) to %s", len(files), describe(el)), nil
		})
	},
}

var (
	waitText string
	waitURL  string
	waitGone string
)

var waitCmd = &cobra.Command{
	Use:   "wait [target]",
	Short: "Wait until an element is visible, or --text appears, --url matches, --gone disappears",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && waitText == "" && waitURL == "" && waitGone == "" {
			return errors.New("give a target, --text, --url or --gone")
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			deadline := time.Now().Add(timeout - 2*time.Second)
			for {
				ok, what := waitCheck(p, args)
				if ok {
					fmt.Fprintln(stdout, what)
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("gave up waiting: %s", what)
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
	},
}

func waitCheck(p *rod.Page, args []string) (bool, string) {
	if len(args) == 1 {
		el, err := element(p, args[0])
		if err != nil {
			return false, args[0] + " not present"
		}
		if v, err := el.Visible(); err != nil || !v {
			return false, args[0] + " not visible"
		}
	}
	if waitText != "" {
		res, err := p.Eval(`(t) => document.body && document.body.innerText.toLowerCase().includes(t.toLowerCase())`, waitText)
		if err != nil || !res.Value.Bool() {
			return false, fmt.Sprintf("text %q not on page", waitText)
		}
	}
	if waitURL != "" {
		info, err := p.Info()
		if err != nil || !strings.Contains(info.URL, waitURL) {
			return false, fmt.Sprintf("url does not contain %q", waitURL)
		}
	}
	if waitGone != "" {
		if el, err := element(p, waitGone); err == nil {
			if v, err := el.Visible(); err == nil && v {
				return false, waitGone + " still visible"
			}
		}
	}
	return true, "ok"
}

func init() {
	for _, c := range []*cobra.Command{clickCmd, hoverCmd, fillCmd, typeCmd, pressCmd, scrollCmd, uploadCmd} {
		c.Flags().BoolVarP(&actSnap, "snap", "s", false, "print a full snapshot of the page afterwards (instead of the change report)")
		c.Flags().BoolVar(&noDiff, "no-diff", false, "skip the signals/changes report")
		c.Flags().IntVar(&snapMax, "max", 400, "snapshot item limit (with --snap)")
	}
	clickCmd.Flags().BoolVar(&clickJS, "js", false, "dispatch element.click() in JS (ignores overlays, no real pointer)")
	clickCmd.Flags().BoolVar(&clickDouble, "double", false, "double click")
	clickCmd.Flags().BoolVar(&clickRight, "right", false, "right click")
	typeCmd.Flags().StringVar(&typeInto, "into", "", "focus this element first")
	typeCmd.Flags().BoolVar(&typeSubmit, "submit", false, "press Enter after typing")
	waitCmd.Flags().StringVar(&waitText, "text", "", "page text contains (case-insensitive)")
	waitCmd.Flags().StringVar(&waitURL, "url", "", "page URL contains")
	waitCmd.Flags().StringVar(&waitGone, "gone", "", "element (ref or selector) is absent or hidden")
	rootCmd.AddCommand(clickCmd, hoverCmd, fillCmd, typeCmd, pressCmd, scrollCmd, uploadCmd, waitCmd)
}

// humanScrollCmd scrolls with the wheel. "bottom" keeps going while the page
// grows (infinite lists), so lazy content gets loaded the way a reader would.
func humanScrollCmd(s *session, p *rod.Page, where string, px int) (string, error) {
	pos := func() (float64, float64, float64) {
		res, err := p.Eval(`() => [scrollY, document.scrollingElement.scrollHeight, innerHeight]`)
		if err != nil {
			return 0, 0, 0
		}
		a := res.Value.Arr()
		return a[0].Num(), a[1].Num(), a[2].Num()
	}
	y, h, vh := pos()
	switch where {
	case "down", "up":
		d := float64(px)
		if d == 0 {
			d = vh * (0.6 + rand.Float64()*0.3)
		}
		if where == "up" {
			d = -d
		}
		if err := s.humanWheel(p, d); err != nil {
			return "", err
		}
	case "top":
		if err := s.humanWheel(p, -y); err != nil {
			return "", err
		}
	case "bottom":
		stuck := 0
		for i := 0; i < 60 && stuck < 3; i++ {
			if err := s.humanWheel(p, vh*(0.6+rand.Float64()*0.3)); err != nil {
				return "", err
			}
			pause(300*time.Millisecond, 900*time.Millisecond)
			ny, nh, _ := pos()
			if ny+vh >= nh-2 && nh == h {
				stuck++
			} else {
				stuck = 0
			}
			y, h = ny, nh
		}
	}
	y, h, _ = pos()
	return fmt.Sprintf("scroll %d/%d (human)", int(y), int(h)), nil
}
