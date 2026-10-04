package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/spf13/cobra"
)

// Long infinite lists (feeds, search results, connection lists) keep every
// loaded item in the DOM; past a few thousand the tab slows down and may
// crash. trim drops items already scrolled past; revive reopens a crashed tab.

const trimJS = `function (keep, hide) {
  // The main list: the element with the most children that mostly share
  // one tag+class signature.
  let best = null, bestN = 0;
  for (const el of document.querySelectorAll('body *')) {
    const n = el.childElementCount;
    if (n < 20 || n <= bestN) continue;
    const sig = new Map();
    for (const c of el.children) {
      const k = c.tagName + '.' + (typeof c.className === 'string' ? c.className : '');
      sig.set(k, (sig.get(k) || 0) + 1);
    }
    if (Math.max(...sig.values()) >= n * 0.6) { best = el; bestN = n; }
  }
  if (!best) return { trimmed: 0, total: 0, container: '' };
  const kids = [...best.children].filter((c) => c.style.display !== 'none');
  let trimmed = 0, freed = 0;
  for (let i = 0; i < kids.length - keep; i++) {
    const c = kids[i];
    const r = c.getBoundingClientRect();
    if (r.bottom > 0) break; // only items fully above the viewport
    freed += r.height;
    if (hide) c.style.display = 'none'; else c.remove();
    trimmed++;
  }
  // Content above shrank by 'freed'; keep what is on screen where it was.
  if (freed) window.scrollBy(0, -freed);
  const id = best.id ? '#' + best.id : '';
  const cls = typeof best.className === 'string' && best.className ? '.' + best.className.trim().split(/\s+/)[0] : '';
  return { trimmed, total: kids.length, container: best.tagName.toLowerCase() + id + cls };
}`

type pageMetrics struct{ nodes, heapMB float64 }

func metrics(p *rod.Page) pageMetrics {
	_ = proto.PerformanceEnable{}.Call(p)
	res, err := proto.PerformanceGetMetrics{}.Call(p)
	var m pageMetrics
	if err != nil {
		return m
	}
	for _, x := range res.Metrics {
		switch x.Name {
		case "Nodes":
			m.nodes = x.Value
		case "JSHeapUsedSize":
			m.heapMB = x.Value / 1e6
		}
	}
	return m
}

var (
	trimKeep int
	trimHide bool
)

var trimCmd = &cobra.Command{
	Use:   "trim",
	Short: "Drop list items already scrolled past, so long infinite lists stay light",
	Long: "Finds the page's main repeating list and removes items fully above the\n" +
		"viewport, keeping the last --keep, and keeps the visible position. Use it\n" +
		"between scrolls when harvesting a long feed; read/extract items before\n" +
		"trimming them. --hide hides instead of removing (safer for fragile apps,\n" +
		"frees layout work but not memory). It cannot shrink the app's own JS\n" +
		"state, so some apps still grow; 'oko revive' recovers a crashed tab.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			before := metrics(p)
			raw, err := evalJSON(p, trimJS, trimKeep, trimHide)
			if err != nil {
				return err
			}
			var r struct {
				Trimmed, Total int
				Container      string
			}
			_ = json.Unmarshal(raw, &r)
			if r.Container == "" {
				fmt.Fprintln(stdout, "no long list found (needs a container with 20+ similar items)")
				return nil
			}
			_ = proto.HeapProfilerCollectGarbage{}.Call(p)
			after := metrics(p)
			verb := "removed"
			if trimHide {
				verb = "hid"
			}
			fmt.Fprintf(stdout, "%s %d of %d items in %s; DOM nodes %.0f → %.0f, JS heap %.0f → %.0f MB\n",
				verb, r.Trimmed, r.Total, r.Container, before.nodes, after.nodes, before.heapMB, after.heapMB)
			return nil
		})
	},
}

var reviveCmd = &cobra.Command{
	Use:   "revive [id]",
	Short: "Reopen a crashed or hung tab at the same URL (default: current tab)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			ts, err := s.tabs()
			if err != nil {
				return err
			}
			id := s.prof.State.Tab
			if len(args) == 1 {
				id = args[0]
			} else if tabFlag != "" {
				id = tabFlag
			}
			if id == "" {
				return fmt.Errorf("no current tab; give a tab id from 'oko tabs'")
			}
			t, err := matchTab(ts, id)
			if err != nil {
				return err
			}
			// Target-level calls work even when the renderer is gone.
			np, err := s.b.Page(proto.TargetCreateTarget{URL: t.URL, Background: true})
			if err != nil {
				return err
			}
			_, _ = proto.TargetCloseTarget{TargetID: t.TargetID}.Call(s.b)
			s.prof.State.Tab = string(np.TargetID)
			_ = s.prof.Save()
			fmt.Fprintf(stdout, "revived %s as %s: %s\n", shortID(t.TargetID), shortID(np.TargetID), t.URL)
			return nil
		})
	},
}

func init() {
	trimCmd.Flags().IntVar(&trimKeep, "keep", 30, "always keep this many items at the end of the list")
	trimCmd.Flags().BoolVar(&trimHide, "hide", false, "hide items instead of removing them")
	rootCmd.AddCommand(trimCmd, reviveCmd)
}
