package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

var sizeRe = regexp.MustCompile(`^(\d+)x(\d+)$`)

var (
	vpMobile bool
	vpScale  float64
)

// Desktop Chrome windows cannot be narrower than about 500px, so smaller or
// mobile viewports use device emulation. Emulation lives only as long as the
// CDP session that set it, hence a detached holder process per tab.
const minWindowWidth = 500

var viewportCmd = &cobra.Command{
	Use:   "viewport <WxH|reset>",
	Short: "Set the page viewport: window resize, or device emulation for phones (--mobile / width < 500)",
	Long: "Widths of 500 and up resize the window (no background process).\n" +
		"--mobile or a narrower width emulates a device: exact size, mobile layout,\n" +
		"touch, device pixel ratio (--scale, default 3 with --mobile). A small\n" +
		"background 'oko' process keeps the emulation alive until 'oko viewport reset'\n" +
		"or the tab closes.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] == "reset" {
			return run(func(s *session) error {
				p, err := s.page()
				if err != nil {
					return err
				}
				stopHolder(s.prof, p.TargetID)
				_ = proto.EmulationClearDeviceMetricsOverride{}.Call(p)
				res, err := p.Eval(`() => innerWidth + 'x' + innerHeight`)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "viewport %s (window)\n", res.Value.Str())
				return nil
			})
		}
		m := sizeRe.FindStringSubmatch(args[0])
		if m == nil {
			return fmt.Errorf("size must look like 1280x800 or 'reset'")
		}
		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		return run(func(s *session) error {
			p, err := s.page()
			if err != nil {
				return err
			}
			stopHolder(s.prof, p.TargetID)
			if vpMobile || w < minWindowWidth {
				scale := vpScale
				if scale == 0 {
					scale = 1
					if vpMobile {
						scale = 3
					}
				}
				return startHolder(s, p, w, h, scale, vpMobile)
			}
			return resizeWindow(s, p, w, h)
		})
	},
}

func resizeWindow(s *session, p *rod.Page, w, h int) error {
	win, err := proto.BrowserGetWindowForTarget{TargetID: p.TargetID}.Call(s.b)
	if err != nil {
		return err
	}
	_ = proto.BrowserSetWindowBounds{WindowID: win.WindowID, Bounds: &proto.BrowserBounds{WindowState: proto.BrowserWindowStateNormal}}.Call(s.b)
	// Window chrome (tab strip, toolbar) eats part of the window, so size
	// the window, measure the viewport, and correct.
	set := func(ww, wh int) error {
		return proto.BrowserSetWindowBounds{WindowID: win.WindowID, Bounds: &proto.BrowserBounds{Width: &ww, Height: &wh}}.Call(s.b)
	}
	measure := func() (int, int, error) {
		res, err := p.Eval(`() => [window.innerWidth, window.innerHeight]`)
		if err != nil {
			return 0, 0, err
		}
		arr := res.Value.Arr()
		return arr[0].Int(), arr[1].Int(), nil
	}
	if err := set(w, h); err != nil {
		return err
	}
	vw, vh := 0, 0
	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		if vw, vh, err = measure(); err != nil {
			return err
		}
		if vw == w && vh == h {
			break
		}
		if err := set(w+(w-vw), h+(h-vh)); err != nil {
			return err
		}
	}
	time.Sleep(150 * time.Millisecond)
	if vw, vh, err = measure(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "viewport %dx%d (window)\n", vw, vh)
	return nil
}

func startHolder(s *session, p *rod.Page, w, h int, scale float64, mobile bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"_hold", "--profile", s.prof.Name, string(p.TargetID),
		strconv.Itoa(w), strconv.Itoa(h), strconv.FormatFloat(scale, 'f', -1, 64), strconv.FormatBool(mobile)}
	c := exec.Command(self, args...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	pid := c.Process.Pid
	go func() { _ = c.Wait() }()

	// Wait until the emulation is visible from our own session.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		// screen size, not innerWidth: with mobile emulation a page without a
		// viewport meta tag lays out at 980px, exactly like a real phone.
		res, err := p.Eval(`() => screen.width + 'x' + screen.height`)
		if err == nil && res.Value.Str() == fmt.Sprintf("%dx%d", w, h) {
			if s.prof.State.Holders == nil {
				s.prof.State.Holders = map[string]int{}
			}
			s.prof.State.Holders[string(p.TargetID)] = pid
			_ = s.prof.Save()
			mode := "emulated"
			if mobile {
				mode = fmt.Sprintf("emulated mobile, touch, dpr %g", scale)
			}
			fmt.Fprintf(stdout, "viewport %dx%d (%s; 'oko viewport reset' to undo)\n", w, h, mode)
			return nil
		}
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	return fmt.Errorf("device emulation did not take effect")
}

func stopHolder(prof *chrome.Profile, tab proto.TargetTargetID) {
	pid := prof.State.Holders[string(tab)]
	if pid == 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	delete(prof.State.Holders, string(tab))
	_ = prof.Save()
	time.Sleep(100 * time.Millisecond)
}

// holdCmd is the background process behind emulated viewports: it applies
// device metrics on its own CDP session and stays attached until the tab
// closes, the browser goes away, or it is signalled.
var holdCmd = &cobra.Command{
	Use:    "_hold <tab> <w> <h> <scale> <mobile>",
	Hidden: true,
	Args:   cobra.ExactArgs(5),
	RunE: func(cmd *cobra.Command, args []string) error {
		w, _ := strconv.Atoi(args[1])
		h, _ := strconv.Atoi(args[2])
		scale, _ := strconv.ParseFloat(args[3], 64)
		mobile := args[4] == "true"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := connect(ctx, false)
		if err != nil {
			return err
		}
		tabFlag = args[0]
		p, err := s.page()
		if err != nil {
			return err
		}
		if err := (proto.EmulationSetDeviceMetricsOverride{Width: w, Height: h, DeviceScaleFactor: scale, Mobile: mobile}).Call(p); err != nil {
			return err
		}
		if mobile {
			_ = proto.EmulationSetTouchEmulationEnabled{Enabled: true, MaxTouchPoints: gsonInt(5)}.Call(p)
		}
		_ = proto.TargetSetDiscoverTargets{Discover: true}.Call(s.b)
		s.b.EachEvent(func(e *proto.TargetTargetDestroyed) bool {
			return string(e.TargetID) == args[0]
		})()
		return nil
	},
}

func gsonInt(n int) *int { return &n }

func init() {
	viewportCmd.Flags().BoolVar(&vpMobile, "mobile", false, "emulate a phone: mobile layout, touch, dpr 3")
	viewportCmd.Flags().Float64Var(&vpScale, "scale", 0, "device pixel ratio for emulation")
	rootCmd.AddCommand(viewportCmd, holdCmd)
}
