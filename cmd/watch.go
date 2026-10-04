package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

// The watcher is a detached `oko _watch` process per profile. It keeps one
// CDP connection to the browser, puts the element picker into every tab
// (isolated world, so the page cannot see or forge it), and turns picks into
// records in the profile's picks/ dir: snapshot ref, component + source file,
// crop. 'oko picks' and the UserPromptSubmit hook read those records.

func watchPidPath(p *chrome.Profile) string { return filepath.Join(p.Dir, "watch.pid") }
func watchLogPath(p *chrome.Profile) string { return filepath.Join(p.Dir, "watch.log") }
func watchReadyPath(p *chrome.Profile) string {
	return filepath.Join(p.Dir, "watch.ready")
}

// watchPID returns the pid of the profile's live watcher, or 0.
func watchPID(p *chrome.Profile) int {
	b, err := os.ReadFile(watchPidPath(p))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

// ensureWatch starts the watcher when the browser runs without one. It never
// fails the calling command; wait makes it block until the picker is in.
func ensureWatch(p *chrome.Profile, wait bool) error {
	// Headless has nobody to pick; OKO_WATCH=1 forces it (tests).
	force := os.Getenv("OKO_WATCH") == "1"
	if p.State.NoWatch || os.Getenv("OKO_WATCH") == "0" || p.State.Headless && !force {
		return errors.New("watcher disabled for this profile ('oko watch on' enables it)")
	}
	if watchPID(p) != 0 {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.Remove(watchReadyPath(p))
	logf, err := os.OpenFile(watchLogPath(p), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	c := exec.Command(self, "_watch", "--profile", p.Name)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdout, c.Stderr = logf, logf
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	if !wait {
		return nil
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(watchReadyPath(p)); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("watcher did not start; see %s", watchLogPath(p))
}

// cdpTarget sends raw CDP calls on one attached session. The watcher does not
// use rod pages: rod enables the Page domain, which would change how the
// user's own dialogs and navigations behave while the watcher holds the tab.
type cdpTarget struct {
	b   *rod.Browser
	ctx context.Context
	sid proto.TargetSessionID
	tab proto.TargetTargetID
}

func (t *cdpTarget) Call(ctx context.Context, _, method string, params interface{}) ([]byte, error) {
	return t.b.Call(ctx, string(t.sid), method, params)
}
func (t *cdpTarget) GetSessionID() proto.TargetSessionID { return t.sid }
func (t *cdpTarget) GetContext() context.Context         { return t.ctx }

type watcher struct {
	prof *chrome.Profile
	b    *rod.Browser
	ctx  context.Context

	mu      sync.Mutex
	targets map[proto.TargetSessionID]*cdpTarget
	byTab   map[proto.TargetTargetID]*cdpTarget
	worlds  map[proto.TargetSessionID]proto.RuntimeExecutionContextID
	busy    map[proto.TargetSessionID]bool
}

// isolated evaluates expr in the tab's picker world.
func (w *watcher) isolated(t *cdpTarget, expr string) (*proto.RuntimeEvaluateResult, error) {
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	tc := &cdpTarget{b: t.b, ctx: ctx, sid: t.sid, tab: t.tab}
	world, err := proto.PageCreateIsolatedWorld{FrameID: proto.PageFrameID(t.tab), WorldName: chrome.PickWorld}.Call(tc)
	if err != nil {
		return nil, err
	}
	return proto.RuntimeEvaluate{Expression: expr, ContextID: world.ExecutionContextID, ReturnByValue: true}.Call(tc)
}

func (w *watcher) attach(info *proto.TargetTargetInfo) {
	if info.Type != proto.TargetTargetInfoTypePage || strings.HasPrefix(info.URL, "devtools://") || strings.HasPrefix(info.URL, "chrome-extension://") {
		return
	}
	w.mu.Lock()
	if _, ok := w.byTab[info.TargetID]; ok {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	res, err := proto.TargetAttachToTarget{TargetID: info.TargetID, Flatten: true}.Call(w.b.Context(ctx))
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach %s: %v\n", shortID(info.TargetID), err)
		return
	}
	t := &cdpTarget{b: w.b, ctx: w.ctx, sid: res.SessionID, tab: info.TargetID}
	w.mu.Lock()
	w.targets[t.sid] = t
	w.byTab[t.tab] = t
	w.mu.Unlock()
	tc := &cdpTarget{b: w.b, ctx: ctx, sid: t.sid, tab: t.tab}
	if _, err := (proto.PageAddScriptToEvaluateOnNewDocument{Source: chrome.PickerJS, WorldName: chrome.PickWorld, RunImmediately: true}).Call(tc); err != nil {
		fmt.Fprintf(os.Stderr, "init script %s: %v\n", shortID(t.tab), err)
	}
	w.ensureBinding(t)
	w.applyConsumed(t)
}

// ensureBinding gives the tab's current picker world the __okoPick binding.
// Chrome installs a binding into contexts created later only while the
// Runtime domain is enabled, and enabling it is what bot checks look for
// (console argument serialization). So each new document's world gets the
// binding installed when the watcher notices it: on navigation events and a
// slow poll. The picker queues anything it sends before that and resends
// on flush.
func (w *watcher) ensureBinding(t *cdpTarget) {
	w.mu.Lock()
	if w.busy[t.sid] {
		w.mu.Unlock()
		return
	}
	w.busy[t.sid] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.busy, t.sid)
		w.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	tc := &cdpTarget{b: t.b, ctx: ctx, sid: t.sid, tab: t.tab}
	world, err := proto.PageCreateIsolatedWorld{FrameID: proto.PageFrameID(t.tab), WorldName: chrome.PickWorld}.Call(tc)
	if err != nil {
		return
	}
	w.mu.Lock()
	same := w.worlds[t.sid] == world.ExecutionContextID
	w.mu.Unlock()
	if same {
		return
	}
	_ = proto.RuntimeRemoveBinding{Name: chrome.PickBinding}.Call(tc)
	if err := (proto.RuntimeAddBinding{Name: chrome.PickBinding, ExecutionContextName: chrome.PickWorld}).Call(tc); err != nil {
		fmt.Fprintf(os.Stderr, "binding %s: %v\n", shortID(t.tab), err)
		return
	}
	// The init script normally put the picker there already; a world we
	// created first (or a document from before the watcher) needs it now.
	if _, err := (proto.RuntimeEvaluate{Expression: chrome.PickerJS + ";window.__okoPicker && window.__okoPicker.flush()", ContextID: world.ExecutionContextID}).Call(tc); err != nil {
		return
	}
	w.mu.Lock()
	w.worlds[t.sid] = world.ExecutionContextID
	w.mu.Unlock()
}

func (w *watcher) detach(sid proto.TargetSessionID, tab proto.TargetTargetID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.targets[sid]; ok {
		delete(w.byTab, t.tab)
		delete(w.targets, sid)
		delete(w.worlds, sid)
	}
	if t, ok := w.byTab[tab]; ok {
		delete(w.targets, t.sid)
		delete(w.byTab, tab)
		delete(w.worlds, t.sid)
	}
}

type pickPayload struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	N      int    `json:"n"`
	On     bool   `json:"on"`
	Picked int    `json:"picked"`
	Note   string `json:"note"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Role   string `json:"role"`
	Name   string `json:"name"`
	Ctx    string `json:"ctx"`
	Sel    string `json:"sel"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Pad    string `json:"pad"`
	BG     string `json:"bg"`
	Color  string `json:"color"`
	Font   string `json:"font"`
}

func validPickID(id string) bool {
	if len(id) == 0 || len(id) > 16 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (w *watcher) handle(t *cdpTarget, raw string) {
	var pl pickPayload
	if json.Unmarshal([]byte(raw), &pl) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 8*time.Second)
	defer cancel()
	tc := &cdpTarget{b: t.b, ctx: ctx, sid: t.sid, tab: t.tab}
	switch pl.T {
	case "arm":
		if _, err := (proto.RuntimeEvaluate{Expression: chrome.MainSourceJS}).Call(tc); err != nil {
			fmt.Fprintf(os.Stderr, "arm %s: %v\n", shortID(t.tab), err)
		}
	case "mode":
		_ = w.prof.SetPickMode(chrome.PickMode{Tab: string(t.tab), On: pl.On, Picked: pl.Picked, At: time.Now()})
	case "pick":
		if !validPickID(pl.ID) {
			return
		}
		w.pick(tc, pl)
		if _, err := w.isolated(t, fmt.Sprintf("window.__okoPicker && window.__okoPicker.done(%q)", pl.ID)); err != nil {
			fmt.Fprintf(os.Stderr, "done %s: %v\n", shortID(t.tab), err)
		}
	}
}

func (w *watcher) pick(tc *cdpTarget, pl pickPayload) {
	tab := string(tc.tab)
	pk := chrome.Pick{
		ID: pl.ID, At: time.Now(), Tab: tab, URL: pl.URL, Title: pl.Title, N: pl.N,
		Role: pl.Role, Name: pl.Name, Ctx: pl.Ctx, Sel: pl.Sel,
		W: pl.W, H: pl.H, X: pl.X, Y: pl.Y, Pad: pl.Pad, BG: pl.BG, Color: pl.Color, Font: pl.Font,
		Note: pl.Note,
	}
	// Ref, source and crop come from the main world, where the page's refs
	// and framework internals live.
	base := 0
	if p, err := chrome.Load(w.prof.Name); err == nil {
		base = p.State.Seq[tab]
	}
	expr := fmt.Sprintf("(%s)(%q, %d)", chrome.PickResolveJS, pl.ID, base)
	res, err := proto.RuntimeEvaluate{Expression: expr, ReturnByValue: true}.Call(tc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve %s: %v\n", pl.ID, err)
	} else if res.ExceptionDetails != nil {
		fmt.Fprintf(os.Stderr, "resolve %s: %s\n", pl.ID, res.ExceptionDetails.Text)
	} else if res.Result != nil && !res.Result.Value.Nil() {
		var r struct {
			Ref  string  `json:"ref"`
			Seq  int     `json:"seq"`
			Comp string  `json:"comp"`
			File string  `json:"file"`
			X    float64 `json:"x"`
			Y    float64 `json:"y"`
			W    float64 `json:"w"`
			H    float64 `json:"h"`
		}
		_ = json.Unmarshal([]byte(res.Result.Value.JSON("", "")), &r)
		pk.Ref, pk.Comp, pk.File = r.Ref, r.Comp, r.File
		if r.Seq > base {
			// Fresh load: other oko commands write state.json too.
			if p, err := chrome.Load(w.prof.Name); err == nil {
				if p.State.Seq == nil {
					p.State.Seq = map[string]int{}
				}
				if r.Seq > p.State.Seq[tab] {
					p.State.Seq[tab] = r.Seq
					_ = p.Save()
				}
			}
		}
		if r.W >= 1 && r.H >= 1 {
			pk.Shot = w.crop(tc, pl.ID, r.X, r.Y, r.W, r.H)
		}
	}
	if err := w.prof.AddPick(pk); err != nil {
		fmt.Fprintf(os.Stderr, "save pick: %v\n", err)
	}
}

// crop screenshots the element (plus a little context) into picks/<id>.png.
func (w *watcher) crop(tc *cdpTarget, id string, x, y, wd, ht float64) string {
	const pad = 6
	if ht > 1600 {
		ht = 1600
	}
	if wd > 2400 {
		wd = 2400
	}
	x, y = x-pad, y-pad
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	shot, err := proto.PageCaptureScreenshot{
		Format: proto.PageCaptureScreenshotFormatPng,
		Clip:   &proto.PageViewport{X: x, Y: y, Width: wd + 2*pad, Height: ht + 2*pad, Scale: 1},
	}.Call(tc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crop %s: %v\n", id, err)
		return ""
	}
	path := filepath.Join(w.prof.PicksDir(), id+".png")
	if err := os.WriteFile(path, shot.Data, 0o600); err != nil {
		return ""
	}
	return path
}

// applyConsumed greys out read picks and drops cleared ones in one tab.
func (w *watcher) applyConsumed(t *cdpTarget) {
	consumed := w.prof.Consumed()
	if len(consumed) == 0 {
		return
	}
	var spent, gone []string
	for _, pk := range w.prof.Picks() {
		if pk.Tab != string(t.tab) {
			continue
		}
		switch by, ok := consumed[pk.ID]; {
		case !ok:
		case by == chrome.ClearedBy:
			gone = append(gone, pk.ID)
		default:
			spent = append(spent, pk.ID)
		}
	}
	if len(spent)+len(gone) == 0 {
		return
	}
	sj, _ := json.Marshal(spent)
	gj, _ := json.Marshal(gone)
	_, _ = w.isolated(t, fmt.Sprintf("window.__okoPicker && (window.__okoPicker.spent(%s), window.__okoPicker.remove(%s))", sj, gj))
}

func (w *watcher) all() []*cdpTarget {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*cdpTarget, 0, len(w.targets))
	for _, t := range w.targets {
		out = append(out, t)
	}
	return out
}

func (w *watcher) run() error {
	events := w.b.Event()
	if err := (proto.TargetSetDiscoverTargets{Discover: true}).Call(w.b); err != nil {
		return err
	}
	res, err := proto.TargetGetTargets{}.Call(w.b)
	if err != nil {
		return err
	}
	for _, info := range res.TargetInfos {
		w.attach(info)
	}
	_ = os.WriteFile(watchReadyPath(w.prof), nil, 0o600)
	fmt.Fprintf(os.Stderr, "%s watching %d tabs\n", time.Now().Format(time.RFC3339), len(w.all()))

	// Documents that appeared without a target event (same-URL reloads,
	// history navigations) still get the binding within a second.
	go func() {
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(800 * time.Millisecond):
			}
			for _, t := range w.all() {
				go w.ensureBinding(t)
			}
		}
	}()

	// Read picks turn grey in the page: follow consumed.json.
	go func() {
		var last time.Time
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(400 * time.Millisecond):
			}
			fi, err := os.Stat(w.prof.ConsumedPath())
			if err != nil || !fi.ModTime().After(last) {
				continue
			}
			last = fi.ModTime()
			for _, t := range w.all() {
				w.applyConsumed(t)
			}
		}
	}()

	for {
		var msg *rod.Message
		select {
		case <-w.ctx.Done():
			return nil
		case m, ok := <-events:
			if !ok {
				return nil
			}
			msg = m
		}
		switch msg.Method {
		case "Target.targetCreated":
			var e proto.TargetTargetCreated
			if msg.Load(&e) && e.TargetInfo != nil {
				go w.attach(e.TargetInfo)
			}
		case "Target.targetInfoChanged":
			// Usually a navigation: the new document needs the binding.
			var e proto.TargetTargetInfoChanged
			if msg.Load(&e) && e.TargetInfo != nil {
				w.mu.Lock()
				t := w.byTab[e.TargetInfo.TargetID]
				w.mu.Unlock()
				if t != nil {
					go w.ensureBinding(t)
				}
			}
		case "Target.targetDestroyed":
			var e proto.TargetTargetDestroyed
			if msg.Load(&e) {
				w.detach("", e.TargetID)
			}
		case "Target.detachedFromTarget":
			var e proto.TargetDetachedFromTarget
			if msg.Load(&e) {
				w.detach(e.SessionID, "")
			}
		case "Runtime.bindingCalled":
			var e proto.RuntimeBindingCalled
			if !msg.Load(&e) || e.Name != chrome.PickBinding {
				continue
			}
			w.mu.Lock()
			t := w.targets[proto.TargetSessionID(msg.SessionID)]
			w.mu.Unlock()
			if t != nil {
				go w.handle(t, e.Payload)
			}
		}
	}
}

var watcherCmd = &cobra.Command{
	Use:    "_watch",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := chrome.Load(profileName)
		if err != nil {
			return err
		}
		// One watcher per profile, even when two commands spawn at once.
		lf, err := os.OpenFile(filepath.Join(p.Dir, "watch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		defer lf.Close()
		if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := connect(ctx, false)
		if err != nil {
			return err
		}
		_ = os.WriteFile(watchPidPath(p), []byte(strconv.Itoa(os.Getpid())), 0o600)
		defer os.Remove(watchPidPath(p))
		defer os.Remove(watchReadyPath(p))

		w := &watcher{prof: p, b: s.b, ctx: ctx,
			targets: map[proto.TargetSessionID]*cdpTarget{}, byTab: map[proto.TargetTargetID]*cdpTarget{},
			worlds: map[proto.TargetSessionID]proto.RuntimeExecutionContextID{}, busy: map[proto.TargetSessionID]bool{}}

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		go func() { <-sig; cancel() }()
		// The event stream ends when Chrome goes away; also check, since a
		// half-open socket may never close.
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
				_, err := proto.BrowserGetVersion{}.Call(s.b.Context(pctx))
				pcancel()
				if err != nil && ctx.Err() == nil {
					fmt.Fprintln(os.Stderr, "browser gone:", err)
					cancel()
					return
				}
			}
		}()
		err = w.run()
		if ctx.Err() != nil {
			return nil
		}
		return err
	},
}

var watchCmd = &cobra.Command{
	Use:   "watch [status|start|stop|on|off]",
	Short: "The element picker: Alt+P in the browser, picks reach the agent ('oko picks')",
	Long: "The watcher is a small background process per profile that puts the element\n" +
		"picker into every tab. In the browser: Alt+P toggles pick mode, click picks an\n" +
		"element (with an optional note), Shift+click picks and stays in the mode,\n" +
		"Alt+click picks without it, ArrowUp selects the parent, Esc leaves.\n" +
		"It starts with the browser; 'off' keeps it from starting for this profile.",
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"status", "start", "stop", "on", "off"},
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := chrome.Load(profileName)
		if err != nil {
			return err
		}
		what := "status"
		if len(args) == 1 {
			what = args[0]
		}
		stop := func() {
			if pid := watchPID(p); pid != 0 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
				for i := 0; i < 30 && watchPID(p) != 0; i++ {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		switch what {
		case "status":
			if pid := watchPID(p); pid != 0 {
				fmt.Fprintf(stdout, "watching: profile %s, pid %d (Alt+P in the browser)\n", p.Name, pid)
			} else if p.State.NoWatch {
				fmt.Fprintf(stdout, "off for profile %s ('oko watch on' enables it)\n", p.Name)
			} else {
				fmt.Fprintf(stdout, "not running (starts with the next oko command, or 'oko watch start')\n")
			}
			return nil
		case "stop":
			stop()
			fmt.Fprintln(stdout, "stopped (starts again with the next oko command; 'oko watch off' keeps it off)")
			return nil
		case "off":
			p.State.NoWatch = true
			_ = p.Save()
			stop()
			fmt.Fprintf(stdout, "off for profile %s\n", p.Name)
			return nil
		case "on", "start":
			if what == "on" {
				p.State.NoWatch = false
				_ = p.Save()
			}
			return run(func(s *session) error {
				if err := ensureWatch(s.prof, true); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "watching: profile %s, pid %d (Alt+P in the browser)\n", s.prof.Name, watchPID(s.prof))
				return nil
			})
		}
		return fmt.Errorf("unknown %q: use status, start, stop, on or off", what)
	},
}

func init() {
	rootCmd.AddCommand(watchCmd, watcherCmd)
}
