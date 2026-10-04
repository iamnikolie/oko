package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

type snapItem struct {
	Kind  string                 `json:"kind"`
	Ref   string                 `json:"ref,omitempty"`
	Role  string                 `json:"role,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Level int                    `json:"level,omitempty"`
	Attrs map[string]interface{} `json:"attrs,omitempty"`
	Ctx   string                 `json:"ctx,omitempty"`
}

type snapResult struct {
	Title   string     `json:"title"`
	URL     string     `json:"url"`
	ScrollY int        `json:"scrollY"`
	ScrollH int        `json:"scrollH"`
	VW      int        `json:"vw"`
	VH      int        `json:"vh"`
	Seq     int        `json:"seq"`
	Modal   bool       `json:"modal,omitempty"`
	Items   []snapItem `json:"items"`
	Scope   string     `json:"scope"`
	Signals *signals   `json:"signals,omitempty"`
	Prev    *struct {
		Scope string     `json:"scope"`
		Items []snapItem `json:"items"`
	} `json:"prev,omitempty"`
}

type signals struct {
	Messages []string `json:"messages"`
	Invalid  []struct {
		Ref   string `json:"ref"`
		Name  string `json:"name"`
		Error string `json:"error"`
	} `json:"invalid"`
	Loading int `json:"loading"`
}

// line renders signals as one compact line ("" when there are none).
func (sg *signals) line() string {
	if sg == nil {
		return ""
	}
	var parts []string
	for _, m := range sg.Messages {
		parts = append(parts, fmt.Sprintf("message %q", m))
	}
	for _, iv := range sg.Invalid {
		p := fmt.Sprintf("invalid [%s] %q", iv.Ref, iv.Name)
		if iv.Error != "" {
			p += fmt.Sprintf(": %q", iv.Error)
		}
		parts = append(parts, p)
	}
	if sg.Loading > 0 {
		parts = append(parts, fmt.Sprintf("loading (%d indicators)", sg.Loading))
	}
	return strings.Join(parts, " | ")
}

// itemKey identifies an item across snapshots: refs for elements, content
// for headings and markers (they have no refs).
func itemKey(it snapItem) string {
	if it.Kind == "el" {
		return it.Ref
	}
	return it.Kind + "\x00" + it.Role + "\x00" + it.Name
}

// diffLines compares the previous full snapshot with the current one.
func diffLines(prev, cur []snapItem) []string {
	old := map[string]snapItem{}
	for _, it := range prev {
		if it.Kind != "text" {
			old[itemKey(it)] = it
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, it := range cur {
		if it.Kind == "text" {
			continue
		}
		k := itemKey(it)
		seen[k] = true
		o, ok := old[k]
		if !ok {
			out = append(out, "+ "+renderItem(it))
			continue
		}
		// Focus moves on every click; it is not a change worth reporting.
		a := strings.Replace(renderItem(o), " focused", "", 1)
		b := strings.Replace(renderItem(it), " focused", "", 1)
		if a != b {
			out = append(out, "~ "+b+"   (was: "+strings.TrimPrefix(a, "["+it.Ref+"] ")+")")
		}
	}
	for _, it := range prev {
		if it.Kind == "text" || seen[itemKey(it)] {
			continue
		}
		out = append(out, "- "+renderItem(it))
	}
	return out
}

type snapOpts struct {
	text     bool
	viewport bool
	within   string
	max      int
}

var (
	snapText     bool
	snapViewport bool
	snapWithin   string
	snapMax      int
)

// takeSnapshot runs the snapshot script and decodes it.
func takeSnapshot(p *rod.Page, o snapOpts, diff bool) (*snapResult, error) {
	prof, _ := chrome.Load(profileName)
	base := 0
	if prof != nil {
		base = prof.State.Seq[string(p.TargetID)]
	}
	args := map[string]interface{}{"text": o.text, "viewport": o.viewport, "seqBase": base, "diff": diff}
	raw, err := evalJSON(p, chrome.SnapshotJS, args)
	if err != nil {
		return nil, err
	}
	var r snapResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if prof != nil && r.Seq != base {
		if prof.State.Seq == nil {
			prof.State.Seq = map[string]int{}
		}
		prof.State.Seq[string(p.TargetID)] = r.Seq
		_ = prof.Save()
	}
	return &r, nil
}

func snapshot(p *rod.Page, o snapOpts) error {
	prof, _ := chrome.Load(profileName)
	base := 0
	if prof != nil {
		base = prof.State.Seq[string(p.TargetID)]
	}
	args := map[string]interface{}{"text": o.text, "viewport": o.viewport, "seqBase": base}
	var raw []byte
	if o.within != "" {
		el, err := element(p, o.within)
		if err != nil {
			return err
		}
		res, err := el.Eval(chrome.SnapshotJS, args)
		if err != nil {
			return err
		}
		raw, _ = json.Marshal(res.Value)
	} else {
		var err error
		if raw, err = evalJSON(p, chrome.SnapshotJS, args); err != nil {
			return err
		}
	}
	var r snapResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if prof != nil && r.Seq != base {
		if prof.State.Seq == nil {
			prof.State.Seq = map[string]int{}
		}
		prof.State.Seq[string(p.TargetID)] = r.Seq
		_ = prof.Save()
	}
	more := 0
	if o.max > 0 && len(r.Items) > o.max {
		more = len(r.Items) - o.max
		r.Items = r.Items[:o.max]
	}
	if jsonOutput {
		return printJSON(map[string]interface{}{"tab": shortID(p.TargetID), "page": r, "truncated": more})
	}
	fmt.Fprintf(stdout, "page: %s\nurl: %s\nview: %dx%d, scroll %d/%d\n", r.Title, r.URL, r.VW, r.VH, r.ScrollY, r.ScrollH)
	if r.Modal {
		fmt.Fprintln(stdout, "modal open: showing only the dialog (Escape or its close button to leave)")
	}
	if l := r.Signals.line(); l != "" {
		fmt.Fprintln(stdout, "signals: "+l)
	}
	fmt.Fprintln(stdout)
	for _, it := range r.Items {
		fmt.Fprintln(stdout, renderItem(it))
	}
	if len(r.Items) == 0 {
		fmt.Fprintln(stdout, "(no interactive elements; try --text, or 'oko text')")
	}
	if o.within == "" {
		fs, frs := frameSnapshots(p, o)
		for i, fr := range frs {
			fmt.Fprintf(stdout, "-- frame f%d %q (%s) --\n", i+1, fs[i].Title, fs[i].Origin)
			if fr == nil {
				fmt.Fprintln(stdout, "  (could not read this frame)")
				continue
			}
			for _, it := range fr.Items {
				fmt.Fprintln(stdout, renderItem(it))
			}
		}
	}
	if more > 0 {
		fmt.Fprintf(stdout, "… %d more (narrow with --in <ref|selector>, --viewport, or raise --max)\n", more)
	}
	return nil
}

func renderItem(it snapItem) string {
	switch it.Kind {
	case "heading":
		lvl := it.Level
		if lvl < 1 || lvl > 6 {
			lvl = 2
		}
		return strings.Repeat("#", lvl) + " " + it.Name
	case "marker":
		if it.Name != "" {
			return fmt.Sprintf("-- %s %q --", it.Role, it.Name)
		}
		return "-- " + it.Role + " --"
	case "text":
		return "  " + it.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", it.Ref, it.Role)
	if it.Name != "" {
		fmt.Fprintf(&b, " %q", it.Name)
	}
	a := it.Attrs
	if v, ok := a["value"].(string); ok {
		fmt.Fprintf(&b, " = %q", v)
	}
	if v, ok := a["checked"].(bool); ok {
		if v {
			b.WriteString(" checked")
		} else {
			b.WriteString(" unchecked")
		}
	}
	if v, ok := a["expanded"].(bool); ok {
		if v {
			b.WriteString(" expanded")
		} else {
			b.WriteString(" collapsed")
		}
	}
	for _, k := range []string{"selected", "disabled", "focused"} {
		if v, ok := a[k].(bool); ok && v {
			b.WriteString(" " + k)
		}
	}
	if v, ok := a["href"].(string); ok {
		b.WriteString(" → " + v)
	}
	if it.Ctx != "" {
		fmt.Fprintf(&b, " (in: %s)", it.Ctx)
	}
	return b.String()
}

var snapCmd = &cobra.Command{
	Use:   "snap",
	Short: "Compact page snapshot: interactive elements with refs, plus headings",
	Long: "Prints one line per interactive element as [ref] role \"name\" plus state, and\n" +
		"headings as markdown. Refs (e12) address elements in click/fill/… and stay valid\n" +
		"while the element lives. --text adds the readable text between them.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			return snapshot(p, snapOpts{text: snapText, viewport: snapViewport, within: snapWithin, max: snapMax})
		})
	},
}

var textMax int

var textCmd = &cobra.Command{
	Use:   "text [target]",
	Short: "Readable text of the page or one element",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			var res *proto.RuntimeRemoteObject
			if len(args) == 1 {
				el, err := element(p, args[0])
				if err != nil {
					return err
				}
				if res, err = el.Eval(chrome.TextJS); err != nil {
					return err
				}
			} else if res, err = p.Eval(chrome.TextJS); err != nil {
				return err
			}
			t := res.Value.Str()
			if textMax > 0 && len([]rune(t)) > textMax {
				t = string([]rune(t)[:textMax]) + fmt.Sprintf("\n… truncated at %d chars (--max 0 for all)", textMax)
			}
			fmt.Fprintln(stdout, t)
			return nil
		})
	},
}

var htmlMax int

var htmlCmd = &cobra.Command{
	Use:   "html [target]",
	Short: "outerHTML of an element (default: whole document)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			var h string
			if len(args) == 1 {
				el, err := element(p, args[0])
				if err != nil {
					return err
				}
				if h, err = el.HTML(); err != nil {
					return err
				}
			} else if h, err = p.HTML(); err != nil {
				return err
			}
			if htmlMax > 0 && len(h) > htmlMax {
				h = h[:htmlMax] + fmt.Sprintf("\n… truncated at %d bytes (--max 0 for all)", htmlMax)
			}
			fmt.Fprintln(stdout, h)
			return nil
		})
	},
}

var (
	shotOut  string
	shotFull bool
)

var shotCmd = &cobra.Command{
	Use:   "shot [target]",
	Short: "Screenshot the viewport, the full page, or one element; prints the file path",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			// Only a hidden tab needs activating to paint; activating raises
			// Chrome over whatever the user is doing, so avoid it otherwise.
			if r, err := p.Eval(`() => document.visibilityState`); err == nil && r.Value.Str() == "hidden" {
				_ = proto.TargetActivateTarget{TargetID: p.TargetID}.Call(s.b)
			}
			var img []byte
			if len(args) == 1 {
				el, err := element(p, args[0])
				if err != nil {
					return err
				}
				// rod's element screenshot clips with the wrong offset on HiDPI
				// screens; clip by the element's page-absolute box instead.
				if _, err := el.Eval(`function () { this.scrollIntoView({block: 'center'}) }`); err != nil {
					return err
				}
				res, err := el.Eval(`function () { const r = this.getBoundingClientRect(); return [r.left + scrollX, r.top + scrollY, r.width, r.height] }`)
				if err != nil {
					return err
				}
				b := res.Value.Arr()
				if b[2].Num() <= 0 || b[3].Num() <= 0 {
					return fmt.Errorf("element has no visible box")
				}
				shot, err := proto.PageCaptureScreenshot{
					Format:                proto.PageCaptureScreenshotFormatPng,
					Clip:                  &proto.PageViewport{X: b[0].Num(), Y: b[1].Num(), Width: b[2].Num(), Height: b[3].Num(), Scale: 1},
					CaptureBeyondViewport: true,
				}.Call(p)
				if err != nil {
					return err
				}
				img = shot.Data
			} else if img, err = p.Screenshot(shotFull, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng}); err != nil {
				return err
			}
			out := shotOut
			if out == "" {
				dir := filepath.Join(chrome.Home(), "shots")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				out = filepath.Join(dir, time.Now().Format("20060102-150405.000")+".png")
			}
			if err := os.WriteFile(out, img, 0o600); err != nil {
				return err
			}
			abs, _ := filepath.Abs(out)
			fmt.Fprintln(stdout, abs)
			return nil
		})
	},
}

var evalOn string

var evalCmd = &cobra.Command{
	Use:   "eval <js>",
	Short: "Run JavaScript in the page; prints the result as JSON",
	Long: "Accepts an expression ('document.title') or a function ('() => …',\n" +
		"'async () => …', 'el => …'). Promises are awaited. With --on <target> the\n" +
		"function receives the element as its first argument.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		js := strings.TrimSpace(args[0])
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			var res *proto.RuntimeRemoteObject
			if evalOn != "" {
				el, err := element(p, evalOn)
				if err != nil {
					return err
				}
				if res, err = el.Eval(`async function () { return okoPlain(await (` + asFunc(js) + `)(this)); ` + plainJS + ` }`); err != nil {
					return err
				}
			} else if res, err = p.Eval(`async function () { return okoPlain(await (` + asFunc(js) + `)()); ` + plainJS + ` }`); err != nil {
				return err
			}
			if res.Type == proto.RuntimeRemoteObjectTypeUndefined {
				fmt.Fprintln(stdout, "undefined")
				return nil
			}
			// DOM nodes and other non-serializable objects come back without
			// a value; their description ("div.card") is the useful part.
			if res.Value.Nil() && res.Type == proto.RuntimeRemoteObjectTypeObject &&
				res.Subtype != proto.RuntimeRemoteObjectSubtypeNull && res.Description != "" {
				fmt.Fprintln(stdout, res.Description)
				return nil
			}
			if res.Type == proto.RuntimeRemoteObjectTypeString && !jsonOutput {
				fmt.Fprintln(stdout, res.Value.Str())
				return nil
			}
			return printJSON(res.Value)
		})
	},
}

// plainJS turns DOM results, which do not serialize, into readable HTML
// snippets so 'eval "document.querySelector(…)"' shows something useful.
const plainJS = `function okoPlain(r) {
  const node = (n) => {
    if (n.nodeType !== 1) return n.textContent;
    const h = n.outerHTML;
    return h.length > 300 ? h.slice(0, 299) + '…' : h;
  };
  if (r instanceof Node) return node(r);
  if (r instanceof NodeList || r instanceof HTMLCollection) return Array.from(r).slice(0, 50).map(node);
  return r;
}`

var funcArrowRe = regexp.MustCompile(`^(async\s+)?[A-Za-z_$][\w$]*\s*=>`)

var parenArrowRe = regexp.MustCompile(`^(async\s*)?\([^()]*\)\s*=>`)

func isFuncSrc(js string) bool {
	// An immediately invoked function is an expression, not a function.
	t := strings.TrimRight(js, "; \n\t")
	if strings.HasSuffix(t, ")()") {
		return false
	}
	return strings.HasPrefix(js, "function") || strings.HasPrefix(js, "async function") ||
		parenArrowRe.MatchString(js) || funcArrowRe.MatchString(js)
}

// asFunc wraps a bare expression or statement list so every input can be
// called the same way; indirect eval returns the last statement's value.
func asFunc(js string) string {
	if isFuncSrc(js) {
		return js
	}
	q, _ := json.Marshal(js)
	return "() => (0, eval)(" + string(q) + ")"
}

func init() {
	snapCmd.Flags().BoolVar(&snapText, "text", false, "include readable text between elements")
	snapCmd.Flags().BoolVar(&snapViewport, "viewport", false, "only what is currently on screen")
	snapCmd.Flags().StringVar(&snapWithin, "in", "", "only inside this element (ref or selector)")
	snapCmd.Flags().IntVar(&snapMax, "max", 400, "item limit (0 = no limit)")
	textCmd.Flags().IntVar(&textMax, "max", 8000, "character limit (0 = no limit)")
	htmlCmd.Flags().IntVar(&htmlMax, "max", 20000, "byte limit (0 = no limit)")
	shotCmd.Flags().StringVarP(&shotOut, "out", "o", "", "output file (default ~/.oko/shots/<time>.png)")
	shotCmd.Flags().BoolVar(&shotFull, "full", false, "whole scrollable page")
	evalCmd.Flags().StringVar(&evalOn, "on", "", "pass this element (ref or selector) as the function's argument")
	rootCmd.AddCommand(snapCmd, textCmd, htmlCmd, shotCmd, evalCmd)
}
