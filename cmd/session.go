package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
)

type session struct {
	ctx  context.Context
	prof *chrome.Profile
	b    *rod.Browser

	mu    sync.Mutex
	notes []string // dialogs answered during the command
}

var errNotRunning = errors.New("browser not running (start it with 'oko up')")

// connect attaches to the profile's browser, starting it when launch is set
// and nothing answers.
func connect(ctx context.Context, launch bool) (*session, error) {
	p, err := chrome.Load(profileName)
	if err != nil {
		return nil, err
	}
	ws, err := p.Endpoint(ctx)
	if err != nil && p.PID() != 0 {
		// The profile's Chrome is alive but did not answer; it may still be
		// starting or busy. Never launch a second one on the same profile.
		for i := 0; i < 25 && err != nil; i++ {
			time.Sleep(200 * time.Millisecond)
			ws, err = p.Endpoint(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("chrome for profile %s is running (pid %d) but its debugging port does not answer: %v; 'oko down' restarts it", p.Name, p.PID(), err)
		}
	}
	if err != nil {
		if !launch {
			return nil, errNotRunning
		}
		ws, err = p.Launch(ctx, p.State.Headless)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(stderr, "oko: started chrome (profile %s, port %d)\n", p.Name, p.State.Port)
	}
	b := rod.New().ControlURL(ws).NoDefaultDevice().Context(ctx)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("attach to chrome on port %d: %w", p.State.Port, err)
	}
	return &session{ctx: ctx, prof: p, b: b}, nil
}

// run is the common wrapper: timeout, attach (auto-start), run fn.
func run(fn func(s *session) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s, err := connect(ctx, true)
	if err != nil {
		return err
	}
	err = fn(s)
	s.flushNotes()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s (raise --timeout if the page is slow)", timeout)
	}
	return err
}

// tabs lists real page targets (no devtools, no extension pages).
func (s *session) tabs() ([]*proto.TargetTargetInfo, error) {
	res, err := proto.TargetGetTargets{}.Call(s.b)
	if err != nil {
		return nil, err
	}
	var out []*proto.TargetTargetInfo
	for _, t := range res.TargetInfos {
		if t.Type != proto.TargetTargetInfoTypePage {
			continue
		}
		if strings.HasPrefix(t.URL, "devtools://") || strings.HasPrefix(t.URL, "chrome-extension://") {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func shortID(id proto.TargetTargetID) string {
	s := strings.ToLower(string(id))
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

func matchTab(ts []*proto.TargetTargetInfo, prefix string) (*proto.TargetTargetInfo, error) {
	prefix = strings.ToLower(prefix)
	var hit []*proto.TargetTargetInfo
	for _, t := range ts {
		if strings.HasPrefix(strings.ToLower(string(t.TargetID)), prefix) {
			hit = append(hit, t)
		}
	}
	switch len(hit) {
	case 0:
		return nil, fmt.Errorf("no tab %q (see 'oko tabs')", prefix)
	case 1:
		return hit[0], nil
	}
	return nil, fmt.Errorf("tab prefix %q is ambiguous", prefix)
}

// page resolves the tab to act on: --tab, else the remembered current tab,
// else the first tab (opening one if the browser has none).
func (s *session) page() (*rod.Page, error) {
	ts, err := s.tabs()
	if err != nil {
		return nil, err
	}
	var t *proto.TargetTargetInfo
	if tabFlag != "" {
		if t, err = matchTab(ts, tabFlag); err != nil {
			return nil, err
		}
	} else {
		if s.prof.State.Tab != "" {
			t, _ = matchTab(ts, s.prof.State.Tab)
		}
		if t == nil && len(ts) > 0 {
			t = ts[0]
		}
		if t == nil {
			p, err := s.b.Page(proto.TargetCreateTarget{URL: "about:blank", Background: true})
			if err != nil {
				return nil, err
			}
			s.setCurrent(p.TargetID)
			return p, nil
		}
		s.setCurrent(t.TargetID)
	}
	// rod enables the Page domain while attaching, which never returns on a
	// tab frozen by a dialog nobody answered; bound it and say why.
	type attached struct {
		p   *rod.Page
		err error
	}
	ch := make(chan attached, 1)
	go func() {
		p, err := s.b.PageFromTarget(t.TargetID)
		ch <- attached{p, err}
	}()
	var p *rod.Page
	select {
	case a := <-ch:
		if a.err != nil {
			return nil, a.err
		}
		p = a.p
	case <-time.After(attachWait()):
		// Either a JS dialog nobody answered, or a page too busy to respond
		// (huge DOM, long task). Both look the same from here.
		return nil, fmt.Errorf("tab %s did not respond within %s: the page is busy, its renderer crashed (Aw, Snap page), or a JS dialog opened outside oko blocks it; retry, check the window, or 'oko revive' to reopen it", shortID(t.TargetID), attachWait())
	}
	s.watchDialogs(p)
	return p, nil
}

// watchDialogs answers alert/confirm/prompt/beforeunload dialogs as they
// open, per --dialog, and records them for the command's output. A dialog
// left open freezes the tab for every later command, so none is left open.
func (s *session) watchDialogs(p *rod.Page) {
	events := s.b.Event()
	go func() {
		for msg := range events {
			if msg.SessionID != p.SessionID || msg.Method != "Page.javascriptDialogOpening" {
				continue
			}
			var e proto.PageJavascriptDialogOpening
			if !msg.Load(&e) {
				continue
			}
			accept := dialogPolicy == "accept" || e.Type == proto.PageDialogTypeAlert
			_ = proto.PageHandleJavaScriptDialog{Accept: accept, PromptText: dialogText}.Call(p)
			verb := "accepted"
			if !accept {
				verb = "dismissed"
			}
			s.mu.Lock()
			s.notes = append(s.notes, fmt.Sprintf("dialog %s %q → %s", e.Type, e.Message, verb))
			s.mu.Unlock()
		}
	}()
}

func (s *session) flushNotes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notes {
		fmt.Fprintln(stdout, n)
	}
	s.notes = nil
}

// attachWait bounds attaching to a tab: generous for heavy pages, but well
// inside the command timeout so the cause can still be reported.
func attachWait() time.Duration {
	w := timeout / 2
	if w > 20*time.Second {
		w = 20 * time.Second
	}
	if w < 5*time.Second {
		w = 5 * time.Second
	}
	return w
}

func (s *session) setCurrent(id proto.TargetTargetID) {
	if tabFlag != "" || s.prof.State.Tab == string(id) {
		return
	}
	s.prof.State.Tab = string(id)
	_ = s.prof.Save()
}

var refRe = regexp.MustCompile(`^@?((?:f(\d+))?e\d+)$`)

// element resolves a target: snapshot ref (e12 / @e12), text=…, or a CSS
// selector. It does not wait; use 'oko wait' for elements still to appear.
func element(p *rod.Page, target string) (*rod.Element, error) {
	if strings.TrimSpace(target) == "" {
		return nil, errors.New("empty target: give a ref (e12), text=…, or a CSS selector")
	}
	pn := p.Sleeper(rod.NotFoundSleeper)
	var el *rod.Element
	var err error
	switch {
	case refRe.MatchString(target):
		m := refRe.FindStringSubmatch(target)
		ref := m[1]
		var off [2]float64
		if m[2] != "" { // f3e12: element inside cross-origin frame 3
			n, _ := strconv.Atoi(m[2])
			fp, o, ferr := framePage(p, n)
			if ferr != nil {
				return nil, ferr
			}
			pn, off = fp.Sleeper(rod.NotFoundSleeper), o
		}
		el, err = pn.ElementByJS(rod.Eval(chrome.ResolveRefJS, ref))
		if err == nil && m[2] != "" {
			frameOffsets[el] = off
		}
		if isNotFound(err) {
			return nil, fmt.Errorf("ref %s is not on the page any more (page changed); run 'oko snap' again", ref)
		}
	case strings.HasPrefix(target, "text="):
		el, err = pn.ElementByJS(rod.Eval(chrome.FindTextJS, strings.TrimPrefix(target, "text=")))
		if isNotFound(err) {
			return nil, fmt.Errorf("no visible element with text %q", strings.TrimPrefix(target, "text="))
		}
	default:
		el, err = pn.Element(target)
		if isNotFound(err) {
			return nil, fmt.Errorf("no element matches selector %q", target)
		}
	}
	if err != nil {
		return nil, err
	}
	out := el.Sleeper(rod.DefaultSleeper)
	if off, ok := frameOffsets[el]; ok {
		frameOffsets[out] = off
	}
	return out, nil
}

func isNotFound(err error) bool {
	var nf *rod.ElementNotFoundError
	return errors.As(err, &nf)
}

// settle waits for the page to load and its DOM to go quiet. Navigation can
// destroy the context mid-wait, so it retries a few times.
func settle(p *rod.Page) {
	for i := 0; i < 4; i++ {
		if _, err := p.Timeout(5*time.Second).Eval(chrome.SettleJS, 250, 3000); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func evalJSON(p *rod.Page, js string, args ...interface{}) ([]byte, error) {
	res, err := p.Eval(js, args...)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res.Value)
}

func printJSON(v interface{}) error {
	// Round-trip through a generic value: types with their own MarshalJSON
	// (rod's gson) escape <, > and & regardless of the encoder setting.
	if b, err := json.Marshal(v); err == nil {
		var g interface{}
		if json.Unmarshal(b, &g) == nil {
			v = g
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
