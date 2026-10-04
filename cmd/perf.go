package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

// perfObserverJS buffers paint, LCP, layout shifts and long tasks from the
// very start of a page load (installed as an init script before reload).
const perfObserverJS = `(() => {
  const P = window.__okoPerf = { lcp: 0, lcpEl: '', cls: 0, longTasks: [], fcp: 0 };
  const obs = (type, cb) => { try { new PerformanceObserver((l) => l.getEntries().forEach(cb)).observe({ type, buffered: true }); } catch (e) {} };
  obs('largest-contentful-paint', (e) => { P.lcp = e.startTime; const el = e.element; P.lcpEl = el ? (el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + (el.currentSrc ? ' ' + el.currentSrc.slice(0, 80) : '')) : ''; });
  obs('layout-shift', (e) => { if (!e.hadRecentInput) P.cls += e.value; });
  obs('longtask', (e) => P.longTasks.push(Math.round(e.duration)));
  obs('paint', (e) => { if (e.name === 'first-contentful-paint') P.fcp = e.startTime; });
})()`

const perfCollectJS = `function () {
  const P = window.__okoPerf || {};
  const nav = performance.getEntriesByType('navigation')[0] || {};
  const res = performance.getEntriesByType('resource');
  const big = res.map((r) => ({ url: r.name, kb: Math.round((r.transferSize || r.encodedBodySize || 0) / 1024), ms: Math.round(r.duration), type: r.initiatorType }))
    .sort((a, b) => b.kb - a.kb).slice(0, 5);
  const total = res.reduce((s, r) => s + (r.transferSize || 0), 0) + (nav.transferSize || 0);
  const tbt = (P.longTasks || []).reduce((s, d) => s + Math.max(0, d - 50), 0);
  return {
    ttfb: Math.round(nav.responseStart || 0), fcp: Math.round(P.fcp || 0), lcp: Math.round(P.lcp || 0), lcpEl: P.lcpEl || '',
    cls: Math.round((P.cls || 0) * 1000) / 1000, dcl: Math.round(nav.domContentLoadedEventEnd || 0), load: Math.round(nav.loadEventEnd || 0),
    longTasks: (P.longTasks || []).length, tbt: Math.round(tbt), requests: res.length + 1, totalKB: Math.round(total / 1024), big,
  };
}`

type vitals struct {
	TTFB, FCP, LCP    int
	LcpEl             string
	CLS               float64
	DCL, Load         int
	LongTasks, TBT    int
	Requests, TotalKB int
	Big               []struct {
		URL  string `json:"url"`
		KB   int    `json:"kb"`
		MS   int    `json:"ms"`
		Type string `json:"type"`
	}
}

// grade marks a metric against the Web Vitals thresholds.
func grade(v, good, poor float64) string {
	switch {
	case v <= good:
		return "good"
	case v <= poor:
		return "needs work"
	}
	return "poor"
}

var perfCmd = &cobra.Command{
	Use:   "perf",
	Short: "Reload the page and measure load: Web Vitals, long tasks, weight, memory",
	Long: "Reloads the current tab with observers installed and reports TTFB, FCP,\n" +
		"LCP (and its element), CLS, long tasks / total blocking time, request count\n" +
		"and weight, the largest resources, DOM nodes and JS heap. Subcommands: trace,\n" +
		"heap, lighthouse. Measures this browser on this machine and network.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			if _, err := (proto.PageAddScriptToEvaluateOnNewDocument{Source: perfObserverJS}).Call(p); err != nil {
				return err
			}
			if err := p.Reload(); err != nil {
				return err
			}
			_ = p.Timeout(timeout - 5*time.Second).WaitLoad()
			time.Sleep(2 * time.Second) // late LCP candidates, shifts, long tasks
			raw, err := evalJSON(p, perfCollectJS)
			if err != nil {
				return err
			}
			var v vitals
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			m := metrics(p)
			if jsonOutput {
				return printJSON(map[string]interface{}{"vitals": v, "domNodes": m.nodes, "jsHeapMB": m.heapMB})
			}
			info, _ := p.Info()
			if info != nil {
				fmt.Fprintf(stdout, "%s\n", info.URL)
			}
			fmt.Fprintf(stdout, "TTFB  %5d ms   %s\n", v.TTFB, grade(float64(v.TTFB), 800, 1800))
			fmt.Fprintf(stdout, "FCP   %5d ms   %s\n", v.FCP, grade(float64(v.FCP), 1800, 3000))
			fmt.Fprintf(stdout, "LCP   %5d ms   %s", v.LCP, grade(float64(v.LCP), 2500, 4000))
			if v.LcpEl != "" {
				fmt.Fprintf(stdout, "  (%s)", v.LcpEl)
			}
			fmt.Fprintln(stdout)
			fmt.Fprintf(stdout, "CLS   %5.3f      %s\n", v.CLS, grade(v.CLS, 0.1, 0.25))
			fmt.Fprintf(stdout, "TBT   %5d ms   %s  (%d long tasks)\n", v.TBT, grade(float64(v.TBT), 200, 600), v.LongTasks)
			fmt.Fprintf(stdout, "DOMContentLoaded %d ms, load %d ms\n", v.DCL, v.Load)
			fmt.Fprintf(stdout, "%d requests, %d KB transferred; DOM nodes %.0f, JS heap %.1f MB\n", v.Requests, v.TotalKB, m.nodes, m.heapMB)
			if len(v.Big) > 0 {
				fmt.Fprintln(stdout, "largest:")
				for _, b := range v.Big {
					fmt.Fprintf(stdout, "  %5d KB %5d ms  %s  %s\n", b.KB, b.MS, b.Type, trunc(b.URL, 100))
				}
			}
			return nil
		})
	},
}

var (
	traceOut      string
	traceReload   bool
	traceDuration time.Duration
)

var perfTraceCmd = &cobra.Command{
	Use:   "trace",
	Short: "Record a performance trace (open in DevTools Performance or ui.perfetto.dev)",
	Long: "Records --duration (default 5s) of the current tab, or a full reload with\n" +
		"--reload, saves the Chrome trace JSON and summarizes the longest main-thread\n" +
		"tasks. Act with oko from another call meanwhile to trace an interaction.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if timeout < traceDuration+30*time.Second {
			timeout = traceDuration + 30*time.Second
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			cats := []string{"devtools.timeline", "disabled-by-default-devtools.timeline", "disabled-by-default-devtools.timeline.frame",
				"v8.execute", "blink.user_timing", "loading", "latencyInfo", "disabled-by-default-v8.cpu_profiler"}
			done := make(chan *proto.TracingTracingComplete, 1)
			events := s.b.Event()
			go func() {
				for msg := range events {
					if msg.Method == "Tracing.tracingComplete" {
						var e proto.TracingTracingComplete
						if msg.Load(&e) {
							done <- &e
							return
						}
					}
				}
			}()
			if err := (proto.TracingStart{
				TransferMode: proto.TracingStartTransferModeReturnAsStream,
				TraceConfig:  &proto.TracingTraceConfig{IncludedCategories: cats},
			}).Call(p); err != nil {
				return err
			}
			if traceReload {
				_ = p.Reload()
				_ = p.Timeout(traceDuration + 10*time.Second).WaitLoad()
				time.Sleep(time.Second)
			} else {
				time.Sleep(traceDuration)
			}
			if err := (proto.TracingEnd{}).Call(p); err != nil {
				return err
			}
			var tc *proto.TracingTracingComplete
			select {
			case tc = <-done:
			case <-time.After(20 * time.Second):
				return errors.New("trace did not complete")
			}
			var buf bytes.Buffer
			for {
				r, err := proto.IORead{Handle: tc.Stream}.Call(p)
				if err != nil {
					return err
				}
				if r.Base64Encoded {
					return errors.New("unexpected binary trace stream")
				}
				buf.WriteString(r.Data)
				if r.EOF {
					break
				}
			}
			_ = proto.IOClose{Handle: tc.Stream}.Call(p)
			out := traceOut
			if out == "" {
				d := filepath.Join(chrome.Home(), "traces")
				if err := os.MkdirAll(d, 0o700); err != nil {
					return err
				}
				out = filepath.Join(d, time.Now().Format("20060102-150405")+".json")
			}
			out, _ = filepath.Abs(out)
			if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s (%.1f MB) — open in DevTools Performance or https://ui.perfetto.dev\n", out, float64(buf.Len())/1e6)
			summarizeTrace(buf.Bytes())
			return nil
		})
	},
}

// summarizeTrace prints main-thread task stats from a trace.
func summarizeTrace(data []byte) {
	var tr struct {
		TraceEvents []struct {
			Name string                 `json:"name"`
			Ph   string                 `json:"ph"`
			Dur  float64                `json:"dur"`
			Args map[string]interface{} `json:"args"`
		} `json:"traceEvents"`
	}
	if json.Unmarshal(data, &tr) != nil {
		return
	}
	var long []float64
	busy := 0.0
	for _, e := range tr.TraceEvents {
		if e.Name == "RunTask" && e.Ph == "X" {
			ms := e.Dur / 1000
			busy += ms
			if ms >= 50 {
				long = append(long, ms)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(long)))
	tbt := 0.0
	for _, l := range long {
		tbt += l - 50
	}
	fmt.Fprintf(stdout, "main thread busy %.0f ms; %d long tasks (≥50 ms), blocking %.0f ms", busy, len(long), tbt)
	if len(long) > 0 {
		var top []string
		for i, l := range long {
			if i == 5 {
				break
			}
			top = append(top, fmt.Sprintf("%.0f", l))
		}
		fmt.Fprintf(stdout, "; longest: %s ms", strings.Join(top, ", "))
	}
	fmt.Fprintln(stdout)
}

var heapOut string

var perfHeapCmd = &cobra.Command{
	Use:   "heap",
	Short: "Save a JS heap snapshot (open in DevTools Memory panel)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if timeout < 2*time.Minute {
			timeout = 2 * time.Minute
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			out := heapOut
			if out == "" {
				d := filepath.Join(chrome.Home(), "heaps")
				if err := os.MkdirAll(d, 0o700); err != nil {
					return err
				}
				out = filepath.Join(d, time.Now().Format("20060102-150405")+".heapsnapshot")
			}
			out, _ = filepath.Abs(out)
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			defer f.Close()
			var mu sync.Mutex
			events := s.b.Event()
			go func() {
				for msg := range events {
					if msg.SessionID != p.SessionID || msg.Method != "HeapProfiler.addHeapSnapshotChunk" {
						continue
					}
					var e proto.HeapProfilerAddHeapSnapshotChunk
					if msg.Load(&e) {
						mu.Lock()
						_, _ = f.WriteString(e.Chunk)
						mu.Unlock()
					}
				}
			}()
			_ = proto.HeapProfilerEnable{}.Call(p)
			_ = proto.HeapProfilerCollectGarbage{}.Call(p)
			if err := (proto.HeapProfilerTakeHeapSnapshot{}).Call(p); err != nil {
				return err
			}
			time.Sleep(300 * time.Millisecond) // last chunks
			mu.Lock()
			defer mu.Unlock()
			st, _ := f.Stat()
			m := metrics(p)
			fmt.Fprintf(stdout, "%s (%.1f MB) — JS heap %.1f MB, DOM nodes %.0f; open in DevTools → Memory → Load\n", out, float64(st.Size())/1e6, m.heapMB, m.nodes)
			return nil
		})
	},
}

var (
	lhMobile bool
	lhOut    string
)

var perfLighthouseCmd = &cobra.Command{
	Use:   "lighthouse",
	Short: "Run Lighthouse on the current URL in this browser (needs Node/npx)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := exec.LookPath("npx"); err != nil {
			return errors.New("lighthouse needs Node.js (npx) on PATH")
		}
		if timeout < 3*time.Minute {
			timeout = 3 * time.Minute
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			info, err := p.Info()
			if err != nil {
				return err
			}
			dir, err := os.MkdirTemp("", "oko-lh-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			base := filepath.Join(dir, "report")
			largs := []string{"-y", "lighthouse", info.URL, fmt.Sprintf("--port=%d", s.prof.State.Port),
				"--output=json", "--output=html", "--output-path=" + base, "--quiet"}
			if !lhMobile {
				largs = append(largs, "--preset=desktop")
			}
			c := exec.Command("npx", largs...)
			var errb bytes.Buffer
			c.Stderr = &errb
			if err := c.Run(); err != nil {
				return fmt.Errorf("lighthouse: %v: %s", err, trunc(strings.TrimSpace(errb.String()), 600))
			}
			raw, err := os.ReadFile(base + ".report.json")
			if err != nil {
				return err
			}
			var rep struct {
				Categories map[string]struct {
					Title string   `json:"title"`
					Score *float64 `json:"score"`
				} `json:"categories"`
				Audits map[string]struct {
					Title        string   `json:"title"`
					Score        *float64 `json:"score"`
					DisplayValue string   `json:"displayValue"`
					Details      struct {
						Type string `json:"type"`
					} `json:"details"`
				} `json:"audits"`
			}
			if err := json.Unmarshal(raw, &rep); err != nil {
				return err
			}
			for _, k := range []string{"performance", "accessibility", "best-practices", "seo"} {
				if c, ok := rep.Categories[k]; ok && c.Score != nil {
					fmt.Fprintf(stdout, "%-15s %3.0f\n", c.Title, *c.Score*100)
				}
			}
			var opp []string
			for _, a := range rep.Audits {
				if a.Details.Type == "opportunity" && a.Score != nil && *a.Score < 0.9 && a.DisplayValue != "" {
					opp = append(opp, fmt.Sprintf("  %s — %s", a.Title, a.DisplayValue))
				}
			}
			sort.Strings(opp)
			if len(opp) > 0 {
				fmt.Fprintln(stdout, "opportunities:")
				for i, o := range opp {
					if i == 8 {
						break
					}
					fmt.Fprintln(stdout, o)
				}
			}
			out := lhOut
			if out == "" {
				d := filepath.Join(chrome.Home(), "lighthouse")
				_ = os.MkdirAll(d, 0o700)
				out = filepath.Join(d, time.Now().Format("20060102-150405")+".html")
			}
			out, _ = filepath.Abs(out)
			if b, err := os.ReadFile(base + ".report.html"); err == nil {
				_ = os.WriteFile(out, b, 0o600)
				fmt.Fprintln(stdout, "report: "+out)
			}
			return nil
		})
	},
}

func init() {
	perfTraceCmd.Flags().StringVarP(&traceOut, "out", "o", "", "output file (default ~/.oko/traces/<time>.json)")
	perfTraceCmd.Flags().BoolVar(&traceReload, "reload", false, "trace a full page reload")
	perfTraceCmd.Flags().DurationVar(&traceDuration, "duration", 5*time.Second, "how long to record (without --reload)")
	perfHeapCmd.Flags().StringVarP(&heapOut, "out", "o", "", "output file (default ~/.oko/heaps/<time>.heapsnapshot)")
	perfLighthouseCmd.Flags().BoolVar(&lhMobile, "mobile", false, "mobile preset (default: desktop)")
	perfLighthouseCmd.Flags().StringVarP(&lhOut, "out", "o", "", "HTML report path (default ~/.oko/lighthouse/<time>.html)")
	perfCmd.AddCommand(perfTraceCmd, perfHeapCmd, perfLighthouseCmd)
	rootCmd.AddCommand(perfCmd)
}
