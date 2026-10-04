package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/spf13/cobra"
)

var atRe = regexp.MustCompile(`^(-?\d+),(-?\d+)$`)

var winAt string

var windowStates = map[string]proto.BrowserWindowState{
	"max":        proto.BrowserWindowStateMaximized,
	"maximize":   proto.BrowserWindowStateMaximized,
	"maximized":  proto.BrowserWindowStateMaximized,
	"full":       proto.BrowserWindowStateFullscreen,
	"fullscreen": proto.BrowserWindowStateFullscreen,
	"min":        proto.BrowserWindowStateMinimized,
	"minimize":   proto.BrowserWindowStateMinimized,
	"minimized":  proto.BrowserWindowStateMinimized,
	"normal":     proto.BrowserWindowStateNormal,
	"restore":    proto.BrowserWindowStateNormal,
}

var windowCmd = &cobra.Command{
	Use:   "window [max|fullscreen|normal|min|WxH] [--at X,Y]",
	Short: "Show or set the window of your current tab: maximize, fullscreen, restore, minimize, outer size, position",
	Long: "No argument prints the window state, outer bounds and the viewport.\n" +
		"WxH sets the outer window size (use 'oko viewport' for an exact page\n" +
		"viewport); --at X,Y moves it. Sizing or moving a maximized/fullscreen\n" +
		"window restores it first. On macOS fullscreen opens a separate Space.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			state  proto.BrowserWindowState
			w, h   int
			x, y   int
			sized  bool
			placed bool
		)
		if len(args) == 1 {
			if st, ok := windowStates[args[0]]; ok {
				state = st
			} else if m := sizeRe.FindStringSubmatch(args[0]); m != nil {
				w, _ = strconv.Atoi(m[1])
				h, _ = strconv.Atoi(m[2])
				sized = true
			} else {
				return fmt.Errorf("want max, fullscreen, normal, min or a size like 1440x900")
			}
		}
		if winAt != "" {
			m := atRe.FindStringSubmatch(winAt)
			if m == nil {
				return fmt.Errorf("--at must look like 0,0")
			}
			x, _ = strconv.Atoi(m[1])
			y, _ = strconv.Atoi(m[2])
			placed = true
		}
		if state != "" && placed && state != proto.BrowserWindowStateNormal {
			return fmt.Errorf("--at only works with a normal window")
		}
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			win, err := proto.BrowserGetWindowForTarget{TargetID: p.TargetID}.Call(s.b)
			if err != nil {
				return err
			}
			id := win.WindowID
			set := func(b proto.BrowserBounds) error {
				return proto.BrowserSetWindowBounds{WindowID: id, Bounds: &b}.Call(s.b)
			}
			// Chrome only moves between non-normal states via normal, and
			// refuses bounds on a maximized/fullscreen/minimized window.
			changing := state != "" || sized || placed
			if changing && win.Bounds.WindowState != proto.BrowserWindowStateNormal && win.Bounds.WindowState != state {
				if err := set(proto.BrowserBounds{WindowState: proto.BrowserWindowStateNormal}); err != nil {
					return err
				}
				time.Sleep(300 * time.Millisecond)
			}
			if state != "" && state != proto.BrowserWindowStateNormal {
				if err := set(proto.BrowserBounds{WindowState: state}); err != nil {
					return err
				}
			} else if sized || placed {
				b := proto.BrowserBounds{}
				if sized {
					b.Width, b.Height = &w, &h
				}
				if placed {
					b.Left, b.Top = &x, &y
				}
				if err := set(b); err != nil {
					return err
				}
			}
			if changing {
				// Fullscreen and maximize animate on macOS; report the settled window.
				time.Sleep(700 * time.Millisecond)
			}
			return printWindow(s, p.TargetID)
		})
	},
}

func printWindow(s *session, tab proto.TargetTargetID) error {
	win, err := proto.BrowserGetWindowForTarget{TargetID: tab}.Call(s.b)
	if err != nil {
		return err
	}
	b := win.Bounds
	line := fmt.Sprintf("window %s %dx%d at %d,%d", b.WindowState, deref(b.Width), deref(b.Height), deref(b.Left), deref(b.Top))
	if p, err := s.page(); err == nil {
		if res, err := p.Eval(`() => innerWidth + 'x' + innerHeight + ' (screen ' + screen.availWidth + 'x' + screen.availHeight + ')'`); err == nil {
			line += ", viewport " + res.Value.Str()
		}
	}
	fmt.Fprintln(stdout, line)
	return nil
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

func init() {
	windowCmd.Flags().StringVar(&winAt, "at", "", "move the window's top-left corner to X,Y (screen pixels)")
	rootCmd.AddCommand(windowCmd)
}
