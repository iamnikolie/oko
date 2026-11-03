package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/iamnikolie/oko/extension"
	"github.com/iamnikolie/oko/internal/bridge"
	"github.com/iamnikolie/oko/internal/chrome"
	"github.com/spf13/cobra"
)

// An extension profile drives the user's own Chrome: the oko extension holds
// the tabs the user handed over and a local bridge ("oko _bridge") speaks CDP
// for them. Commands attach to the bridge exactly as they attach to a Chrome
// oko launched; what needs the whole browser is refused up front.

// defaultExtProfile is the profile 'oko ext pair' sets up when -p is not given.
const defaultExtProfile = "mine"

// extWait bounds how long a command waits for the extension to (re)connect.
// The extension retries at once while it holds a tab and every 30 s otherwise.
const extWait = 35 * time.Second

func errExtUnsupported(what string) error {
	return fmt.Errorf("%s is not available on an extension profile: oko drives single tabs of your own Chrome there, "+
		"not the browser (use a launched profile, e.g. -p default)", what)
}

// extUnsupported lists commands that need the whole browser or a process oko
// started, with what to say instead.
var extUnsupported = map[string]string{
	"window":   "'oko window' (it would move and resize your own window)",
	"viewport": "'oko viewport' (it resizes the window or keeps a device-emulation holder)",
	"watch":    "the element picker ('oko watch')",
	"pick":     "the element picker ('oko pick')",
	"picks":    "the element picker ('oko picks')",
}

// checkExtSupport refuses, before anything runs, the commands an extension
// profile cannot serve.
func checkExtSupport(cmd *cobra.Command) error {
	name := cmd.Name()
	if strings.HasPrefix(name, "_") || cmd.Parent() == nil || cmd.HasParent() && cmd.Parent().Name() == "ext" {
		return nil
	}
	switch name {
	case "ext", "skill", "help", "completion", "hook":
		return nil
	}
	p, err := chrome.Load(profileName)
	if err != nil || !p.State.Extension {
		return nil
	}
	if what, ok := extUnsupported[name]; ok {
		return errExtUnsupported(what)
	}
	if name == "up" {
		for _, f := range []string{"headless", "proxy", "lang", "idle", "native-selects"} {
			if cmd.Flags().Changed(f) {
				return errExtUnsupported("'oko up --" + f + "'")
			}
		}
	}
	return nil
}

func extClient() (*bridge.Client, error) { return bridge.NewClient(chrome.Home()) }

// connectExtension attaches to the bridge in place of a launched Chrome.
func connectExtension(ctx context.Context, p *chrome.Profile) (*session, error) {
	c, err := extClient()
	if err != nil {
		return nil, err
	}
	st, err := readyExtension(ctx, c, p)
	if err != nil {
		return nil, err
	}
	_ = st
	b := rod.New().ControlURL(c.CDPURL()).NoDefaultDevice().Context(ctx)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("attach to the oko bridge on port %d: %w", c.Port, err)
	}
	return &session{ctx: ctx, prof: p, b: b, me: p.Session(sessionKey())}, nil
}

// readyExtension starts the bridge when needed and waits for the paired
// extension to be on the line.
func readyExtension(ctx context.Context, c *bridge.Client, p *chrome.Profile) (bridge.Status, error) {
	if err := c.Ensure(ctx); err != nil {
		return bridge.Status{}, err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return st, err
	}
	if !st.Paired {
		return st, fmt.Errorf("profile %s drives your Chrome through the oko extension, but no extension is paired: run 'oko ext pair -p %s'", p.Name, p.Name)
	}
	if st.Connected {
		return st, nil
	}
	fmt.Fprintln(stderr, "oko: waiting for the oko extension to connect (opening its popup connects at once)")
	wctx, cancel := context.WithTimeout(ctx, extWait)
	defer cancel()
	st, err = c.WaitExtension(wctx)
	if err != nil {
		return st, errors.New("the oko extension is not connected: is Chrome running with the oko extension on? Its popup shows the link state")
	}
	return st, nil
}

// stopExtension gives every tab back and ends the bridge ('oko down').
func stopExtension(ctx context.Context) (bool, error) {
	c, err := extClient()
	if err != nil {
		return false, err
	}
	if !c.Running(ctx) {
		return false, nil
	}
	_ = c.ReleaseAll(ctx)
	return true, c.Stop(ctx)
}

// writeExtension writes the embedded extension where Chrome loads it from.
func writeExtension() (string, error) {
	dir := filepath.Join(bridge.Dir(chrome.Home()), "unpacked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	err := fs.WalkDir(extension.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := extension.Files.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, path), b, 0o600)
	})
	return dir, err
}

func printExtStatus(st bridge.Status, profile string) {
	if jsonOutput {
		_ = printJSON(st)
		return
	}
	paired := "not paired ('oko ext pair')"
	if st.Paired {
		paired = "paired with " + st.Origin
	}
	link := "extension not connected"
	if st.Connected {
		link = "extension connected (" + st.Version + ", " + st.Browser + ")"
	}
	newTabs := "may not open tabs"
	if st.AllowNew {
		newTabs = "may open its own tabs"
	}
	fmt.Fprintf(stdout, "profile %s: your Chrome via the oko extension\nbridge 127.0.0.1:%d (pid %d), %s, %s; oko %s\n", profile, st.Port, st.PID, paired, link, newTabs)
	if len(st.Tabs) == 0 {
		fmt.Fprintln(stdout, "tabs handed to oko: none (click the oko icon on a tab → \"Give this tab to oko\")")
		return
	}
	fmt.Fprintf(stdout, "tabs handed to oko: %d\n", len(st.Tabs))
	for _, t := range st.Tabs {
		own := ""
		if t.Own {
			own = " (opened by oko)"
		}
		fmt.Fprintf(stdout, "  %s  %s  %s%s\n", strings.ToLower(t.TargetID[:min(6, len(t.TargetID))]), trunc(t.Title, 50), t.URL, own)
	}
}

// extProfileName is the profile an 'oko ext' subcommand works on: -p or
// $OKO_PROFILE when given, else "mine".
func extProfileName(cmd *cobra.Command) string {
	if cmd.Flags().Changed("profile") || os.Getenv("OKO_PROFILE") != "" {
		return profileName
	}
	return defaultExtProfile
}

var extNoWait bool

var extCmd = &cobra.Command{
	Use:   "ext",
	Short: "Drive your own Chrome through the oko extension: pair, status, release, unpair",
	Long: "The oko extension lets oko drive tabs of your everyday Chrome, which cannot be\n" +
		"started with a debugging port. Pair once ('oko ext pair'), hand a tab over in the\n" +
		"extension popup, then use any command with -p mine.",
}

var extPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair the extension: print a one-time code to type into its popup (profile: mine)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		p, err := chrome.Load(extProfileName(cmd))
		if err != nil {
			return err
		}
		if !p.State.Extension {
			if _, err := os.Stat(p.UserDataDir()); err == nil {
				return fmt.Errorf("profile %s already has a Chrome oko launches; pair the extension on another profile (default: mine)", p.Name)
			}
			p.State.Extension = true
			if err := p.Save(); err != nil {
				return err
			}
		}
		dir, err := writeExtension()
		if err != nil {
			return err
		}
		c, err := extClient()
		if err != nil {
			return err
		}
		if err := c.Ensure(ctx); err != nil {
			return err
		}
		code, err := c.NewCode(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "pairing code: %s   (valid 10 minutes, works once)\n\n", code)
		fmt.Fprintf(stdout, "1. Once: chrome://extensions → Developer mode → Load unpacked → %s\n", dir)
		fmt.Fprintf(stdout, "2. Click the oko icon in the toolbar, type the code, press Pair (port %d).\n", c.Port)
		fmt.Fprintf(stdout, "3. On a tab oko may drive: oko icon → \"Give this tab to oko\". Then: oko -p %s snap\n", p.Name)
		if extNoWait {
			return nil
		}
		fmt.Fprintln(stdout, "\nwaiting for the extension… (Ctrl-C stops waiting; the code stays valid)")
		deadline := time.Now().Add(10 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			st, err := c.Status(ctx)
			if err != nil {
				return err
			}
			if !st.CodeOpen && st.Paired && st.Connected {
				fmt.Fprintf(stdout, "paired: %s (%s). Profile %s now drives your Chrome.\n", st.Origin, st.Browser, p.Name)
				return nil
			}
			if !st.CodeOpen {
				return errors.New("the code was used up or burned by wrong guesses; run 'oko ext pair' again")
			}
		}
		return errors.New("no extension paired within 10 minutes; run 'oko ext pair' again")
	},
}

var extStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Bridge, pairing, extension link and the tabs handed to oko",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := extClient()
		if err != nil {
			return err
		}
		st, err := c.Status(ctx)
		if err != nil {
			fmt.Fprintf(stdout, "bridge not running (port %d); any command on an extension profile starts it\n", c.Port)
			return nil
		}
		printExtStatus(st, extProfileName(cmd))
		return nil
	},
}

var extReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Give every tab back to you (the extension detaches from all of them)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := extClient()
		if err != nil {
			return err
		}
		if err := c.ReleaseAll(ctx); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "released every tab")
		return nil
	},
}

var extUnpairCmd = &cobra.Command{
	Use:   "unpair",
	Short: "Forget the paired extension; it is refused until paired again",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := extClient()
		if err != nil {
			return err
		}
		if err := c.Ensure(ctx); err != nil {
			return err
		}
		_ = c.ReleaseAll(ctx)
		if err := c.Unpair(ctx); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "unpaired; remove the extension in chrome://extensions to undo it completely")
		return nil
	},
}

var extStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Release every tab and stop the bridge",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		ran, err := stopExtension(ctx)
		if err != nil {
			return err
		}
		if !ran {
			fmt.Fprintln(stdout, "bridge not running")
			return nil
		}
		fmt.Fprintln(stdout, "stopped")
		return nil
	},
}

var extPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Write the extension out and print its folder (for Load unpacked)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := writeExtension()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, dir)
		return nil
	},
}

var bridgeCmd = &cobra.Command{
	Use:    "_bridge",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return bridge.Serve(chrome.Home())
	},
}

func init() {
	extPairCmd.Flags().BoolVar(&extNoWait, "no-wait", false, "print the code and return instead of waiting for the extension")
	extCmd.AddCommand(extPairCmd, extStatusCmd, extReleaseCmd, extUnpairCmd, extStopCmd, extPathCmd)
	rootCmd.AddCommand(extCmd, bridgeCmd)
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error { return checkExtSupport(cmd) }
}
