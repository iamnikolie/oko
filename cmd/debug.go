package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

type consoleLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
	Where string `json:"where,omitempty"`
}

var (
	consoleErrors bool
	consoleFollow time.Duration
)

func remoteArg(p *rod.Page, o *proto.RuntimeRemoteObject) string {
	if o == nil {
		return ""
	}
	switch o.Type {
	case proto.RuntimeRemoteObjectTypeString:
		return o.Value.Str()
	case proto.RuntimeRemoteObjectTypeUndefined:
		return "undefined"
	}
	if o.Preview != nil && len(o.Preview.Properties) > 0 {
		return renderPreview(o.Preview)
	}
	// Replayed history carries no preview; serialize the live object.
	if o.ObjectID != "" && o.Type == proto.RuntimeRemoteObjectTypeObject {
		r, err := proto.RuntimeCallFunctionOn{
			ObjectID:            o.ObjectID,
			FunctionDeclaration: `function () { try { const s = JSON.stringify(this); return s && s.length > 400 ? s.slice(0, 399) + '…' : s } catch (e) { return String(this) } }`,
			ReturnByValue:       true,
		}.Call(p)
		if err == nil && r.Result != nil && r.Result.Value.Str() != "" {
			return r.Result.Value.Str()
		}
	}
	if !o.Value.Nil() {
		b, _ := json.Marshal(o.Value)
		return string(b)
	}
	if o.Description != "" {
		return o.Description
	}
	if o.UnserializableValue != "" {
		return string(o.UnserializableValue)
	}
	return string(o.Type)
}

func renderPreview(pv *proto.RuntimeObjectPreview) string {
	var parts []string
	arr := pv.Subtype == proto.RuntimeObjectPreviewSubtypeArray
	for _, pr := range pv.Properties {
		v := pr.Value
		if pr.Type == proto.RuntimePropertyPreviewTypeString {
			v = fmt.Sprintf("%q", v)
		} else if v == "" {
			v = string(pr.Type)
		}
		if arr {
			parts = append(parts, v)
		} else {
			parts = append(parts, pr.Name+": "+v)
		}
	}
	if pv.Overflow {
		parts = append(parts, "…")
	}
	if arr {
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func frameWhere(st *proto.RuntimeStackTrace) string {
	if st == nil || len(st.CallFrames) == 0 {
		return ""
	}
	f := st.CallFrames[0]
	return fmt.Sprintf("%s:%d", f.URL, f.LineNumber+1)
}

var consoleCmd = &cobra.Command{
	Use:   "console",
	Short: "Console messages and uncaught errors of the current tab (history + --follow)",
	Long: "Chrome keeps a page's console history, so this shows what was logged before\n" +
		"oko attached. --follow 10s keeps listening for that long.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if consoleFollow > 0 && timeout < consoleFollow+5*time.Second {
			timeout = consoleFollow + 5*time.Second
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			var mu sync.Mutex
			var lines []consoleLine
			add := func(l consoleLine) {
				if l.Level == "verbose" || l.Level == "debug" && consoleErrors {
					return
				}
				if consoleErrors && l.Level != "error" && l.Level != "exception" && l.Level != "warning" && l.Level != "assert" {
					return
				}
				mu.Lock()
				lines = append(lines, l)
				if consoleFollow > 0 && !jsonOutput {
					fmt.Fprintln(stdout, renderConsole(l))
				}
				mu.Unlock()
			}
			// Chrome replays a page's stored console history when Runtime and
			// Log are enabled, so subscribe first and only then (re)enable.
			// rod's EachEvent enables before it subscribes and loses the replay.
			events := s.b.Event()
			go func() {
				for msg := range events {
					if msg.SessionID != p.SessionID {
						continue
					}
					switch msg.Method {
					case "Runtime.consoleAPICalled":
						var e proto.RuntimeConsoleAPICalled
						if !msg.Load(&e) {
							continue
						}
						var parts []string
						for _, a := range e.Args {
							parts = append(parts, remoteArg(p, a))
						}
						add(consoleLine{Level: string(e.Type), Text: strings.Join(parts, " "), Where: frameWhere(e.StackTrace)})
					case "Runtime.exceptionThrown":
						var e proto.RuntimeExceptionThrown
						if !msg.Load(&e) || e.ExceptionDetails == nil {
							continue
						}
						d := e.ExceptionDetails
						text := d.Text
						if d.Exception != nil && d.Exception.Description != "" {
							text = d.Exception.Description
						}
						where := d.URL
						if where != "" {
							where = fmt.Sprintf("%s:%d", where, d.LineNumber+1)
						}
						add(consoleLine{Level: "exception", Text: text, Where: where})
					case "Log.entryAdded":
						var e proto.LogEntryAdded
						if !msg.Load(&e) || e.Entry == nil {
							continue
						}
						en := e.Entry
						where := en.URL
						if en.LineNumber != nil && where != "" {
							where = fmt.Sprintf("%s:%d", where, *en.LineNumber+1)
						}
						add(consoleLine{Level: string(en.Level), Text: en.Text, Where: where})
					}
				}
			}()
			_ = proto.RuntimeDisable{}.Call(p)
			_ = proto.LogDisable{}.Call(p)
			if err := (proto.RuntimeEnable{}).Call(p); err != nil {
				return err
			}
			_ = proto.LogEnable{}.Call(p)
			if consoleFollow > 0 {
				time.Sleep(consoleFollow)
			} else {
				time.Sleep(400 * time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			if jsonOutput {
				return printJSON(lines)
			}
			if consoleFollow == 0 {
				for _, l := range lines {
					fmt.Fprintln(stdout, renderConsole(l))
				}
			}
			if len(lines) == 0 {
				fmt.Fprintln(stdout, "(no console messages)")
			}
			return nil
		})
	},
}

func renderConsole(l consoleLine) string {
	s := fmt.Sprintf("[%s] %s", l.Level, trunc(strings.TrimSpace(l.Text), 500))
	if l.Where != "" {
		s += "  (" + l.Where + ")"
	}
	return s
}

type netEntry struct {
	URL    string  `json:"url"`
	Method string  `json:"method,omitempty"`
	Type   string  `json:"type"`
	Status int     `json:"status"`
	Error  string  `json:"error,omitempty"`
	MS     int     `json:"ms"`
	Size   int     `json:"size"`
	Start  float64 `json:"-"`
	Body   string  `json:"body,omitempty"`
}

var (
	netFailed bool
	netFilter string
	netLive   time.Duration
	netReload bool
	netBody   bool
	netAll    bool
)

var netCmd = &cobra.Command{
	Use:   "net",
	Short: "Requests of the current tab: history from Resource Timing, or live capture",
	Long: "Without flags: what the page has loaded so far (from performance entries;\n" +
		"cross-origin statuses may read 0). --reload reloads and captures the full\n" +
		"load live with methods, statuses and errors; --live 10s captures whatever\n" +
		"happens for that long. --body adds response bodies of fetch/xhr requests.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if netLive > 0 && timeout < netLive+10*time.Second {
			timeout = netLive + 10*time.Second
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			var entries []netEntry
			if netLive > 0 || netReload {
				entries, err = captureNet(p)
			} else {
				entries, err = perfNet(p)
			}
			if err != nil {
				return err
			}
			var out []netEntry
			for _, e := range entries {
				if netFilter != "" && !strings.Contains(e.URL, netFilter) {
					continue
				}
				if netFailed && e.Error == "" && e.Status < 400 && !(e.Status == 0 && netLive > 0) {
					continue
				}
				if !netAll && !netFailed && isStatic(e) {
					continue
				}
				out = append(out, e)
			}
			if jsonOutput {
				return printJSON(out)
			}
			for _, e := range out {
				fmt.Fprintln(stdout, renderNet(e))
			}
			if len(out) == 0 {
				fmt.Fprintln(stdout, "(no matching requests)")
			}
			hidden := len(entries) - len(out)
			if hidden > 0 && !netAll && !netFailed {
				fmt.Fprintf(stdout, "(%d other requests hidden: static assets or --filter; --all shows assets)\n", hidden)
			}
			return nil
		})
	},
}

// isStatic hides scripts, styles, images and fonts by default: when an agent
// asks about the network it almost always means documents and API calls.
func isStatic(e netEntry) bool {
	if e.Error != "" || e.Status >= 400 {
		return false
	}
	switch strings.ToLower(e.Type) {
	case "script", "stylesheet", "css", "link", "img", "image", "font", "media", "icon", "manifest", "other":
		return true
	}
	return false
}

func renderNet(e netEntry) string {
	status := fmt.Sprint(e.Status)
	if e.Error != "" {
		status = "ERR " + e.Error
	} else if e.Status == 0 {
		status = "-"
	}
	method := e.Method
	if method == "" {
		method = "   "
	}
	s := fmt.Sprintf("%s %s %s  %s  %dms", status, method, strings.ToLower(e.Type), trunc(e.URL, 140), e.MS)
	if e.Body != "" {
		s += "\n    " + strings.ReplaceAll(trunc(e.Body, 2000), "\n", "\n    ")
	}
	return s
}

func perfNet(p *rod.Page) ([]netEntry, error) {
	raw, err := evalJSON(p, chrome.PerfEntriesJS)
	if err != nil {
		return nil, err
	}
	var es []netEntry
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, err
	}
	return es, nil
}

func captureNet(p *rod.Page) ([]netEntry, error) {
	var mu sync.Mutex
	byID := map[proto.NetworkRequestID]*netEntry{}
	var order []proto.NetworkRequestID
	started := map[proto.NetworkRequestID]time.Time{}
	var loadFired bool
	var lastActivity = time.Now()

	wait := p.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			mu.Lock()
			defer mu.Unlock()
			lastActivity = time.Now()
			if _, ok := byID[e.RequestID]; !ok {
				order = append(order, e.RequestID)
			}
			byID[e.RequestID] = &netEntry{URL: e.Request.URL, Method: e.Request.Method, Type: string(e.Type)}
			started[e.RequestID] = time.Now()
		},
		func(e *proto.NetworkResponseReceived) {
			mu.Lock()
			defer mu.Unlock()
			lastActivity = time.Now()
			if n := byID[e.RequestID]; n != nil {
				n.Status = e.Response.Status
				n.Type = string(e.Type)
			}
		},
		func(e *proto.NetworkLoadingFinished) {
			mu.Lock()
			defer mu.Unlock()
			lastActivity = time.Now()
			if n := byID[e.RequestID]; n != nil {
				n.MS = int(time.Since(started[e.RequestID]).Milliseconds())
				n.Size = int(e.EncodedDataLength)
			}
		},
		func(e *proto.NetworkLoadingFailed) {
			mu.Lock()
			defer mu.Unlock()
			lastActivity = time.Now()
			if n := byID[e.RequestID]; n != nil {
				n.Error = e.ErrorText
				if e.Canceled {
					n.Error = "canceled"
				}
				n.MS = int(time.Since(started[e.RequestID]).Milliseconds())
			}
		},
		func(e *proto.PageLoadEventFired) {
			mu.Lock()
			loadFired = true
			mu.Unlock()
		},
	)
	go wait()
	if err := (proto.NetworkEnable{}).Call(p); err != nil {
		return nil, err
	}
	_ = proto.PageEnable{}.Call(p)

	if netReload {
		if err := p.Reload(); err != nil {
			return nil, err
		}
		// Until load + 1s of network quiet, bounded by --live or the timeout.
		limit := netLive
		if limit == 0 {
			limit = timeout - 5*time.Second
		}
		end := time.Now().Add(limit)
		for time.Now().Before(end) {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			done := loadFired && time.Since(lastActivity) > time.Second
			mu.Unlock()
			if done && netLive == 0 {
				break
			}
		}
	} else {
		time.Sleep(netLive)
	}

	mu.Lock()
	defer mu.Unlock()
	var out []netEntry
	for _, id := range order {
		n := *byID[id]
		if netBody && n.Error == "" && (n.Type == "Fetch" || n.Type == "XHR") &&
			(netFilter == "" || strings.Contains(n.URL, netFilter)) {
			if b, err := (proto.NetworkGetResponseBody{RequestID: id}).Call(p); err == nil {
				if b.Base64Encoded {
					n.Body = fmt.Sprintf("(binary, %d bytes base64)", len(b.Body))
				} else {
					n.Body = b.Body
				}
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func init() {
	consoleCmd.Flags().BoolVar(&consoleErrors, "errors", false, "only errors, exceptions and warnings")
	consoleCmd.Flags().DurationVar(&consoleFollow, "follow", 0, "keep listening this long (e.g. 10s)")
	netCmd.Flags().BoolVar(&netFailed, "failed", false, "only failed requests and HTTP >= 400")
	netCmd.Flags().StringVar(&netFilter, "filter", "", "URL substring")
	netCmd.Flags().DurationVar(&netLive, "live", 0, "capture live for this long (e.g. 10s)")
	netCmd.Flags().BoolVar(&netReload, "reload", false, "reload the page and capture its load")
	netCmd.Flags().BoolVar(&netBody, "body", false, "with --live/--reload: include fetch/xhr response bodies")
	netCmd.Flags().BoolVar(&netAll, "all", false, "include static assets (scripts, styles, images, fonts)")
	rootCmd.AddCommand(consoleCmd, netCmd)
}
