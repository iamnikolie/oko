package cmd

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

var (
	upHeadless bool
	upLang     string
	upProxy    string
	upIdle     string
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the profile's Chrome (other commands do this on demand)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := chrome.Load(profileName)
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("human") {
			p.State.Human = humanFlag
			_ = p.Save()
			fmt.Fprintf(stdout, "profile %s: human-like input %v by default\n", p.Name, humanFlag)
		}
		if upLang != "" {
			p.State.Lang = upLang
			if upLang == "system" {
				p.State.Lang = ""
			}
			_ = p.Save()
		}
		if upIdle != "" {
			if upIdle != "off" && upIdle != "0" {
				if d, err := time.ParseDuration(upIdle); err != nil || d <= 0 {
					return fmt.Errorf("--idle: want a duration like 30m or 2h, or off")
				}
			}
			p.State.Idle = upIdle
			_ = p.Save()
		}
		proxyNote := ""
		if upProxy != "" {
			if upProxy == "none" {
				p.State.Proxy = ""
			} else {
				if _, err := chrome.ParseProxy(upProxy); err != nil {
					return err
				}
				p.State.Proxy = upProxy
			}
			_ = p.Save()
			proxyNote = "proxy: direct"
			if p.State.Proxy != "" {
				proxyNote = "proxy: " + chrome.RedactProxy(p.State.Proxy)
			}
		}
		if _, err := p.Endpoint(ctx); err == nil {
			msg := ""
			if upLang != "" {
				msg = " (language applies after 'oko down' + 'oko up')"
			}
			if proxyNote != "" {
				// A running relay picks the new upstream up on its next
				// connection; a browser started without one needs a restart.
				if p.RelayPort() > 0 {
					msg += "\n" + proxyNote + " (new connections)"
				} else {
					msg += "\n" + proxyNote + " (applies after 'oko down' + 'oko up')"
				}
			}
			_ = ensureWatch(p, false)
			used(p)
			fmt.Fprintf(stdout, "already running: profile %s, port %d, %s%s\n", p.Name, p.State.Port, idleNote(p), msg)
			return nil
		}
		if _, err := p.Launch(ctx, upHeadless); err != nil {
			return err
		}
		_ = ensureWatch(p, false)
		used(p)
		mode := "window"
		if upHeadless {
			mode = "headless"
		}
		fmt.Fprintf(stdout, "started: profile %s, port %d, %s, %s\nprofile dir: %s\n", p.Name, p.State.Port, mode, idleNote(p), p.UserDataDir())
		if p.State.Proxy != "" {
			fmt.Fprintf(stdout, "proxy: %s via relay 127.0.0.1:%d\n", chrome.RedactProxy(p.State.Proxy), p.RelayPort())
		}
		return nil
	},
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Close the profile's Chrome (logins stay in the profile)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := chrome.Load(profileName)
		if err != nil {
			return err
		}
		ran, err := stopBrowser(ctx, p)
		if err != nil {
			return err
		}
		if !ran {
			fmt.Fprintln(stdout, "not running")
			return nil
		}
		fmt.Fprintln(stdout, "stopped")
		return nil
	},
}

// idleNote describes when the browser closes on its own.
func idleNote(p *chrome.Profile) string {
	ttl := p.IdleTTL()
	if ttl == 0 {
		return "never closes when idle"
	}
	return "closes after " + shortDur(ttl) + " idle"
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the profile's Chrome is running",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := chrome.Load(profileName)
		if err != nil {
			return err
		}
		_, err = p.Endpoint(ctx)
		running := err == nil
		me := p.Session(sessionKey())
		if jsonOutput {
			out := map[string]interface{}{"profile": p.Name, "port": p.State.Port, "running": running,
				"headless": p.State.Headless, "dir": p.UserDataDir(), "session": me.Key, "tab": shortTab(me.Tab),
				"idle_limit": p.IdleTTL().String()}
			if t := p.LastUsed(); !t.IsZero() {
				out["last_used"] = t.Format(time.RFC3339)
			}
			if p.State.Proxy != "" {
				out["proxy"] = chrome.RedactProxy(p.State.Proxy)
				out["relay_port"] = p.RelayPort()
			}
			return printJSON(out)
		}
		state := "stopped"
		if running {
			state = "running"
		}
		fmt.Fprintf(stdout, "profile %s: %s, port %d, %s\ndir: %s\n", p.Name, state, p.State.Port, idleNote(p), p.UserDataDir())
		cur := "none"
		if me.Tab != "" {
			cur = shortTab(me.Tab)
		}
		others := map[string]bool{}
		for _, k := range p.Owners() {
			if k != me.Key {
				others[k] = true
			}
		}
		fmt.Fprintf(stdout, "session %s: current tab %s, owns %d; other sessions on this profile: %d\n", shortSession(me.Key), cur, len(me.Opened), len(others))
		if p.State.Proxy != "" {
			relay := "relay not running"
			if port := p.RelayPort(); port > 0 {
				relay = fmt.Sprintf("relay 127.0.0.1:%d", port)
			} else if running {
				relay = "browser started without proxy; 'oko down' + 'oko up'"
			}
			fmt.Fprintf(stdout, "proxy: %s (%s, log %s)\n", chrome.RedactProxy(p.State.Proxy), relay, p.RelayLogPath())
		}
		return nil
	},
}

var tabsCmd = &cobra.Command{
	Use:   "tabs",
	Short: "List tabs with their owner session; * marks your current one",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			ts, err := s.tabs()
			if err != nil {
				return err
			}
			cur := s.me.Tab
			if tabFlag != "" {
				if t, err := matchTab(ts, tabFlag); err == nil {
					cur = string(t.TargetID)
				}
			}
			if jsonOutput {
				var out []map[string]interface{}
				for _, t := range ts {
					out = append(out, map[string]interface{}{"id": shortID(t.TargetID), "title": t.Title, "url": t.URL,
						"current": string(t.TargetID) == cur, "owner": s.ownerLabel(t.TargetID), "mine": s.me.Owns(string(t.TargetID))})
				}
				return printJSON(out)
			}
			for _, t := range ts {
				mark := " "
				if string(t.TargetID) == cur {
					mark = "*"
				}
				fmt.Fprintf(stdout, "%s %s  %-8s  %s  %s\n", mark, shortID(t.TargetID), s.ownerLabel(t.TargetID), trunc(t.Title, 50), t.URL)
			}
			return nil
		})
	},
}

var tabCmd = &cobra.Command{
	Use:   "tab <id>",
	Short: "Make a tab your current one (--front also shows it in the window)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			ts, err := s.tabs()
			if err != nil {
				return err
			}
			t, err := matchTab(ts, args[0])
			if err != nil {
				return err
			}
			if err := s.checkForeign(t.TargetID); err != nil {
				return err
			}
			s.makeCurrent(t.TargetID)
			if tabFront {
				_ = proto.TargetActivateTarget{TargetID: t.TargetID}.Call(s.b)
			}
			fmt.Fprintf(stdout, "%s  %s  %s\n", shortID(t.TargetID), t.Title, t.URL)
			return nil
		})
	},
}

var (
	tabFront  bool
	forceFlag bool
)

var (
	openNew  bool
	openSnap bool
)

var schemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

func normalizeURL(u string) string {
	if schemeRe.MatchString(u) && !strings.HasPrefix(u, "localhost:") {
		return u
	}
	host := strings.SplitN(u, "/", 2)[0]
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "0.0.0.0") {
		return "http://" + u
	}
	return "https://" + u
}

var openCmd = &cobra.Command{
	Use:   "open <url>",
	Short: "Navigate your current tab (or a --new window) and wait for load",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := normalizeURL(args[0])
		return run(func(s *session) error {
			var p *rod.Page
			var err error
			if t := s.startupTab(); openNew && t != nil {
				if p, err = s.b.PageFromTarget(t.TargetID); err != nil {
					return err
				}
				s.watchDialogs(p)
				s.claim(p.TargetID)
				s.setCurrent(p.TargetID)
			} else if openNew {
				p, err = s.b.Page(proto.TargetCreateTarget{URL: "about:blank", NewWindow: true, Background: true})
				if err != nil {
					return err
				}
				s.claim(p.TargetID)
				s.setCurrent(p.TargetID)
			} else {
				s.strict = true
				if p, err = s.page(); err != nil {
					return err
				}
			}
			if err := p.Navigate(url); err != nil {
				return fmt.Errorf("navigate: %w%s", err, proxyHint(s.prof, url, err))
			}
			_ = p.Timeout(timeout - time.Second).WaitLoad()
			settle(p)
			if openSnap {
				return snapshot(p, snapOpts{max: snapMax})
			}
			return printPageLine(p)
		})
	},
}

func printPageLine(p *rod.Page) error {
	info, err := p.Info()
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(map[string]string{"tab": shortID(p.TargetID), "title": info.Title, "url": info.URL})
	}
	fmt.Fprintf(stdout, "%s  %s\n%s\n", shortID(p.TargetID), info.Title, info.URL)
	return nil
}

func historyCmd(use, short string, fn func(p *rod.Page) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(func(s *session) error {
				p, err := s.page()
				if err != nil {
					return err
				}
				// Chrome answers history navigation with "Inspected target
				// navigated or closed" once the navigation has happened.
				if err := fn(p); err != nil && !strings.Contains(err.Error(), "navigated or closed") {
					return err
				}
				_ = p.Timeout(timeout - time.Second).WaitLoad()
				settle(p)
				return printPageLine(p)
			})
		},
	}
}

var closeCmd = &cobra.Command{
	Use:   "close [id]",
	Short: "Close a tab (default: current)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(func(s *session) error {
			var id proto.TargetTargetID
			s.strict = true
			if len(args) == 1 {
				ts, err := s.tabs()
				if err != nil {
					return err
				}
				t, err := matchTab(ts, args[0])
				if err != nil {
					return err
				}
				if err := s.checkForeign(t.TargetID); err != nil {
					return err
				}
				id = t.TargetID
			} else {
				if tabFlag == "" && s.me.Tab == "" && s.me.Key != "default" {
					return fmt.Errorf("this session has no current tab; give a tab id from 'oko tabs'")
				}
				p, err := s.page()
				if err != nil {
					return err
				}
				id = p.TargetID
			}
			if _, err := (proto.TargetCloseTarget{TargetID: id}).Call(s.b); err != nil {
				return err
			}
			if s.me.Tab == string(id) {
				s.me.Tab, s.me.URL = "", ""
			}
			fmt.Fprintf(stdout, "closed %s\n", shortID(id))
			return nil
		})
	},
}

func init() {
	upCmd.Flags().BoolVar(&upHeadless, "headless", false, "run without a window (remembered until 'oko down')")
	upCmd.Flags().StringVar(&upProxy, "proxy", "", "upstream proxy for this profile, remembered: http://user:pass@host:port, https://…, socks5://… ('none' to clear)")
	upCmd.Flags().StringVar(&upIdle, "idle", "", "close the browser after this long without an oko command, remembered (e.g. 30m, 4h; 'off' keeps it open; default 1h)")
	upCmd.Flags().StringVar(&upLang, "lang", "", "browser language for this profile, remembered (e.g. en-US; 'system' to clear)")
	openCmd.Flags().BoolVarP(&openNew, "new", "n", false, "open in a new background window, owned by this session, and make it current")
	openCmd.Flags().BoolVar(&forceFlag, "force", false, "navigate a tab another session owns")
	closeCmd.Flags().BoolVar(&forceFlag, "force", false, "close a tab another session owns")
	tabCmd.Flags().BoolVar(&tabFront, "front", false, "also switch the browser window to this tab (raises Chrome)")
	openCmd.Flags().BoolVarP(&openSnap, "snap", "s", false, "print a snapshot after load")
	openCmd.Flags().IntVar(&snapMax, "max", 400, "snapshot item limit (with --snap)")

	rootCmd.AddCommand(proxyRelayCmd)
	rootCmd.AddCommand(upCmd, downCmd, statusCmd, tabsCmd, tabCmd, openCmd, closeCmd,
		historyCmd("back", "Go back in history", func(p *rod.Page) error { return p.NavigateBack() }),
		historyCmd("forward", "Go forward in history", func(p *rod.Page) error { return p.NavigateForward() }),
		historyCmd("reload", "Reload the page", func(p *rod.Page) error { return p.Reload() }),
	)
}

// proxyRelayCmd is the background relay behind a proxied profile.
var proxyRelayCmd = &cobra.Command{
	Use:    "_proxy",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return chrome.ServeRelay(profileName)
	},
}

// proxyHint explains a tunnel failure on a proxied profile with the relay's
// last complaint (e.g. the upstream's 407), which Chrome does not surface.
func proxyHint(p *chrome.Profile, target string, err error) string {
	if p.State.Proxy == "" || !strings.Contains(err.Error(), "ERR_TUNNEL") && !strings.Contains(err.Error(), "ERR_PROXY") {
		return ""
	}
	b, _ := os.ReadFile(p.RelayLogPath())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	// Prefer the page's own host over Chrome's background requests.
	host := ""
	if u, err := neturl.Parse(target); err == nil {
		host = u.Hostname()
	}
	for _, needle := range []string{"CONNECT " + host + ":", "CONNECT "} {
		for i := len(lines) - 1; i >= 0; i-- {
			if j := strings.Index(lines[i], needle); j >= 0 {
				return "\nproxy relay: " + lines[i][j:]
			}
		}
	}
	return "\nproxy: " + chrome.RedactProxy(p.State.Proxy) + "; see " + p.RelayLogPath()
}
