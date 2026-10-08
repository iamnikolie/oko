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
	// me is the caller: its current tab and the tabs it owns.
	me *chrome.Session
	// owners maps tab id to the owning session key (lazy, see owner).
	owners map[string]string
	// strict makes acting on another session's tab an error instead of a
	// warning; set by commands that navigate or close the tab.
	strict bool
	// looked is set once the command resolved the caller's current tab, so
	// its URL is recorded for the next command's drift check.
	looked bool
	// launched is set when this command started the browser: its startup
	// about:blank tab is free for the taking.
	launched bool

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
	launched := false
	if err != nil {
		if !launch {
			return nil, errNotRunning
		}
		launched = true
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
	return &session{ctx: ctx, prof: p, b: b, me: p.Session(sessionKey()), launched: launched}, nil
}

// run is the common wrapper: timeout, attach (auto-start), run fn.
func run(fn func(s *session) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	s, err := connect(ctx, true)
	if err != nil {
		return err
	}
	// The element picker and the idle reaper ride along with the browser.
	_ = ensureWatch(s.prof, false)
	used(s.prof)
	err = fn(s)
	s.flushNotes()
	s.record()
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

// page resolves the tab to act on: --tab, else this session's current tab,
// else a tab nobody owns (the shared default session) or a fresh one.
func (s *session) page() (*rod.Page, error) {
	ts, err := s.tabs()
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, t := range ts {
		live[string(t.TargetID)] = true
	}
	s.me.Prune(live)
	var t *proto.TargetTargetInfo
	if tabFlag != "" {
		if t, err = matchTab(ts, tabFlag); err != nil {
			return nil, err
		}
		if err := s.checkForeign(t.TargetID); err != nil {
			return nil, err
		}
	} else {
		if s.me.Tab != "" {
			t, _ = matchTab(ts, s.me.Tab)
			if t != nil {
				if err := s.checkForeign(t.TargetID); err != nil {
					return nil, err
				}
				if s.me.URL != "" && t.URL != s.me.URL {
					fmt.Fprintf(stderr, "oko: note: tab %s navigated since your last command (was %s, now %s)\n", shortID(t.TargetID), s.me.URL, t.URL)
					s.me.URL = t.URL
				}
			}
		}
		if t == nil {
			t = s.freeTab(ts)
		}
		if t == nil {
			p, err := s.b.Page(proto.TargetCreateTarget{URL: "about:blank", Background: true})
			if err != nil {
				return nil, err
			}
			s.setCurrent(p.TargetID)
			s.watchDialogs(p)
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

// freeTab picks a tab this session may take without asking: for the shared
// default session the first tab nobody owns (today's single-user behaviour);
// for a named session only an unowned blank tab, so it never lands on a page
// someone else is using.
func (s *session) freeTab(ts []*proto.TargetTargetInfo) *proto.TargetTargetInfo {
	for _, t := range ts {
		if s.owner(t.TargetID) != "" {
			continue
		}
		if s.me.Key == "default" || t.URL == "about:blank" || t.URL == "chrome://newtab/" {
			return t
		}
	}
	return nil
}

// startupTab returns the blank tab Chrome opened at launch when this command
// started the browser, so a first 'open --new' takes it instead of leaving it
// behind in a second window.
func (s *session) startupTab() *proto.TargetTargetInfo {
	if !s.launched {
		return nil
	}
	ts, err := s.tabs()
	if err != nil || len(ts) != 1 {
		return nil
	}
	if t := ts[0]; t.URL == "about:blank" && s.owner(t.TargetID) == "" {
		return t
	}
	return nil
}

// setCurrent makes the tab this session's current one (not with --tab) and
// claims it when nobody owns it.
func (s *session) setCurrent(id proto.TargetTargetID) {
	if tabFlag == "" {
		s.makeCurrent(id)
	}
}

// makeCurrent makes the tab this session's current one, even under --tab.
func (s *session) makeCurrent(id proto.TargetTargetID) {
	s.looked = true
	s.me.Tab = string(id)
	if s.owner(id) == "" {
		s.claim(id)
	}
}

// claim records the tab as opened by this session.
func (s *session) claim(id proto.TargetTargetID) {
	s.me.Own(string(id))
	if s.owners != nil {
		s.owners[string(id)] = s.me.Key
	}
}

// owner returns the key of the other session owning the tab, or "" when the
// tab is this session's or nobody's.
func (s *session) owner(id proto.TargetTargetID) string {
	if s.me.Owns(string(id)) {
		return ""
	}
	if s.owners == nil {
		s.owners = s.prof.Owners()
	}
	if k := s.owners[string(id)]; k != s.me.Key {
		return k
	}
	return ""
}

// checkForeign guards a tab another session owns: an error for commands
// that navigate or close it (unless --force), a warning otherwise.
func (s *session) checkForeign(id proto.TargetTargetID) error {
	o := s.owner(id)
	if o == "" {
		return nil
	}
	if s.strict && !forceFlag {
		return fmt.Errorf("tab %s belongs to session %s; open your own with 'oko open <url> --new', or pass --force", shortID(id), shortSession(o))
	}
	fmt.Fprintf(stderr, "oko: note: tab %s belongs to session %s\n", shortID(id), shortSession(o))
	return nil
}

// ownerLabel names a tab's owner for listings: "you", another session's
// short key, or "-" for nobody.
func (s *session) ownerLabel(id proto.TargetTargetID) string {
	if s.me.Owns(string(id)) {
		return "you"
	}
	if o := s.owner(id); o != "" {
		return shortSession(o)
	}
	return "-"
}

func shortTab(id string) string { return shortID(proto.TargetTargetID(id)) }

func shortSession(k string) string {
	if len(k) > 8 {
		return k[:8]
	}
	return k
}

// record saves this session's state after a command: the URL its current tab
// ended at, for the next command's drift check.
func (s *session) record() {
	if s.looked && s.me.Tab != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if r, err := (proto.TargetGetTargetInfo{TargetID: proto.TargetTargetID(s.me.Tab)}).Call(s.b.Context(ctx)); err == nil {
			s.me.URL = r.TargetInfo.URL
		}
	}
	_ = s.me.Save()
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
