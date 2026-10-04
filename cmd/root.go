package cmd

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"
)

// version is stamped at link time (-X github.com/iamnikolie/oko/cmd.version=…).
var version = "dev"

func buildVersion() string {
	v := version
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
				v += " (" + rev + ")"
			}
		}
	}
	return v
}

var (
	profileName  string
	tabFlag      string
	sessionFlag  string
	jsonOutput   bool
	timeout      time.Duration
	dialogPolicy string
	dialogText   string
)

var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

var rootCmd = &cobra.Command{
	Use:   "oko",
	Short: "Drive a dedicated Chrome from the shell — built for coding agents",
	Long: "oko — browser CLI for agents.\n\n" +
		"Each command attaches to a dedicated Chrome (own profile, own debugging port),\n" +
		"does one thing and exits. The browser is started on first use and keeps its\n" +
		"logins between runs. Run 'oko skill' for the full agent reference.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() {
	rootCmd.Version = buildVersion()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(stderr, "oko:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// sessionKey names the caller: its current tab and the tabs it owns are
// kept apart from other callers on the same profile.
func sessionKey() string {
	if sessionFlag != "" {
		return sessionFlag
	}
	if v := os.Getenv("CLAUDE_CODE_SESSION_ID"); v != "" {
		return v
	}
	return "default"
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&profileName, "profile", "p", envOr("OKO_PROFILE", "default"), "browser profile (own Chrome, logins and port) [$OKO_PROFILE]")
	pf.StringVarP(&tabFlag, "tab", "t", os.Getenv("OKO_TAB"), "tab id prefix to act on instead of the current tab [$OKO_TAB]")
	pf.StringVar(&sessionFlag, "session", os.Getenv("OKO_SESSION"), "caller identity owning a current tab; default $CLAUDE_CODE_SESSION_ID, else shared 'default' [$OKO_SESSION]")
	pf.BoolVar(&jsonOutput, "json", false, "JSON output")
	pf.DurationVar(&timeout, "timeout", 20*time.Second, "overall command timeout")
	pf.StringVar(&dialogPolicy, "dialog", envOr("OKO_DIALOG", "accept"), "answer confirm/prompt/beforeunload dialogs: accept or dismiss [$OKO_DIALOG]")
	pf.StringVar(&dialogText, "prompt-text", "", "text to answer prompt() dialogs with")
	pf.BoolVar(&humanFlag, "human", os.Getenv("OKO_HUMAN") == "1", "human-like input: curved mouse moves, wheel scrolling, typing rhythm [$OKO_HUMAN=1]; 'oko up --human' makes it the profile default")
}
