package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// 'oko hook' wires 'oko picks --hook' into the agents' UserPromptSubmit hooks.
// Claude Code (settings.json) and Codex (hooks.json) share the hook format.
// Edits keep everything else in the file as it was: other keys keep their
// order, other hooks stay, and the old file is kept as <file>.oko.bak.

const hookMarker = "oko picks --hook"

var (
	hookAgents  string
	hookProject bool
	hookFile    string
	hookDryRun  bool
)

type hookTarget struct {
	agent string // "claude" or "codex"
	path  string
}

func (t hookTarget) label() string {
	if t.agent == "claude" {
		return "Claude Code"
	}
	return "Codex"
}

func claudeDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

func codexDir() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// hookTargets resolves which files to edit: --agent (default: every agent
// whose config dir exists), user-wide or --project, or one --file.
func hookTargets() ([]hookTarget, error) {
	var agents []string
	if hookAgents != "" {
		for _, a := range strings.Split(hookAgents, ",") {
			a = strings.TrimSpace(strings.ToLower(a))
			switch a {
			case "claude", "claude-code":
				agents = append(agents, "claude")
			case "codex":
				agents = append(agents, "codex")
			case "all":
				agents = append(agents, "claude", "codex")
			default:
				return nil, fmt.Errorf("unknown agent %q (claude, codex, all)", a)
			}
		}
	} else if hookFile != "" {
		return nil, errors.New("--file needs --agent claude or --agent codex (the file's format)")
	} else if hookProject {
		agents = []string{"claude", "codex"}
	} else {
		if dirExists(claudeDir()) {
			agents = append(agents, "claude")
		}
		if dirExists(codexDir()) {
			agents = append(agents, "codex")
		}
		if len(agents) == 0 {
			return nil, fmt.Errorf("found neither %s nor %s; pass --agent claude or --agent codex", claudeDir(), codexDir())
		}
	}
	if hookFile != "" {
		if len(agents) != 1 {
			return nil, errors.New("--file takes exactly one --agent")
		}
		return []hookTarget{{agents[0], hookFile}}, nil
	}
	var out []hookTarget
	seen := map[string]bool{}
	for _, a := range agents {
		if seen[a] {
			continue
		}
		seen[a] = true
		var p string
		switch {
		case a == "claude" && hookProject:
			p = filepath.Join(".claude", "settings.json")
		case a == "claude":
			p = filepath.Join(claudeDir(), "settings.json")
		case hookProject:
			p = filepath.Join(".codex", "hooks.json")
		default:
			p = filepath.Join(codexDir(), "hooks.json")
		}
		out = append(out, hookTarget{a, p})
	}
	return out, nil
}

// hookCommand is the command the hook runs: the oko on PATH when that is
// this binary (a stable path that survives upgrades), else this binary.
func hookCommand() string {
	bin := "oko"
	self, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		bin = self
		if p, err := exec.LookPath("oko"); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				if real, err := filepath.EvalSymlinks(abs); err == nil && real == self {
					bin = abs
				}
			}
		}
	}
	if strings.ContainsAny(bin, " '\"$`\\") {
		bin = "'" + strings.ReplaceAll(bin, "'", `'\''`) + "'"
	}
	return bin + " picks --hook"
}

// ---- order-preserving JSON object edits

type jsonKV struct {
	k string
	v json.RawMessage
}

func parseObject(b []byte) ([]jsonKV, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out []jsonKV
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, jsonKV{kt.(string), raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func encodeObject(kvs []jsonKV) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range kvs {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.k)
		b.Write(k)
		b.WriteByte(':')
		b.Write(kv.v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func getKey(kvs []jsonKV, k string) (json.RawMessage, int) {
	for i, kv := range kvs {
		if kv.k == k {
			return kv.v, i
		}
	}
	return nil, -1
}

// setKey replaces k, or appends it; a nil value removes it.
func setKey(kvs []jsonKV, k string, v json.RawMessage) []jsonKV {
	_, i := getKey(kvs, k)
	switch {
	case i >= 0 && v == nil:
		return append(kvs[:i], kvs[i+1:]...)
	case i >= 0:
		kvs[i].v = v
		return kvs
	case v != nil:
		return append(kvs, jsonKV{k, v})
	}
	return kvs
}

func isOkoHook(raw json.RawMessage) bool {
	var h struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(raw, &h) == nil && strings.Contains(h.Command, hookMarker)
}

// editHooks rewrites the file's hooks.UserPromptSubmit: drops every oko picks
// hook, then (install) appends one group with cmd. It reports whether an oko
// hook was there and the new file content.
func editHooks(content []byte, install bool, cmd string) (had bool, out []byte, err error) {
	top, err := parseObject(content)
	if err != nil {
		return false, nil, err
	}
	hooksRaw, _ := getKey(top, "hooks")
	hooks, err := parseObject(hooksRaw)
	if err != nil {
		return false, nil, fmt.Errorf("\"hooks\": %w", err)
	}
	upsRaw, _ := getKey(hooks, "UserPromptSubmit")
	var groups []json.RawMessage
	if len(upsRaw) > 0 {
		if err := json.Unmarshal(upsRaw, &groups); err != nil {
			return false, nil, fmt.Errorf("\"hooks.UserPromptSubmit\" is not a list: %w", err)
		}
	}
	var kept []json.RawMessage
	for _, g := range groups {
		obj, err := parseObject(g)
		if err != nil {
			kept = append(kept, g)
			continue
		}
		hsRaw, _ := getKey(obj, "hooks")
		var hs []json.RawMessage
		if json.Unmarshal(hsRaw, &hs) != nil {
			kept = append(kept, g)
			continue
		}
		var keep []json.RawMessage
		for _, h := range hs {
			if isOkoHook(h) {
				had = true
				continue
			}
			keep = append(keep, h)
		}
		if len(keep) == len(hs) {
			kept = append(kept, g)
			continue
		}
		if len(keep) == 0 {
			continue // the group held only oko
		}
		b, _ := json.Marshal(keep)
		kept = append(kept, encodeObject(setKey(obj, "hooks", b)))
	}
	if install {
		c, _ := json.Marshal(cmd)
		h := encodeObject([]jsonKV{{"type", json.RawMessage(`"command"`)}, {"command", c}, {"timeout", json.RawMessage("5")}})
		g := encodeObject([]jsonKV{{"hooks", json.RawMessage("[" + string(h) + "]")}})
		kept = append(kept, g)
	}
	if len(kept) == 0 {
		hooks = setKey(hooks, "UserPromptSubmit", nil)
	} else {
		b, _ := json.Marshal(kept)
		hooks = setKey(hooks, "UserPromptSubmit", b)
	}
	if len(hooks) == 0 {
		top = setKey(top, "hooks", nil)
	} else {
		top = setKey(top, "hooks", encodeObject(hooks))
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, encodeObject(top), "", "  "); err != nil {
		return had, nil, err
	}
	pretty.WriteByte('\n')
	return had, pretty.Bytes(), nil
}

// installedCommand returns the oko hook command in a file, or "".
func installedCommand(content []byte) string {
	var f struct {
		Hooks struct {
			UserPromptSubmit []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"UserPromptSubmit"`
		} `json:"hooks"`
	}
	if json.Unmarshal(content, &f) != nil {
		return ""
	}
	for _, g := range f.Hooks.UserPromptSubmit {
		for _, h := range g.Hooks {
			if strings.Contains(h.Command, hookMarker) {
				return h.Command
			}
		}
	}
	return ""
}

func writeHookFile(path string, old, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if err := os.WriteFile(path+".oko.bak", old, mode); err != nil {
			return err
		}
	}
	tmp := path + ".oko.tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hookEdit(install bool) error {
	targets, err := hookTargets()
	if err != nil {
		return err
	}
	cmd := hookCommand()
	for _, t := range targets {
		old, err := os.ReadFile(t.path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		cur := installedCommand(old)
		switch {
		case install && cur == cmd:
			fmt.Fprintf(stdout, "%s: already installed in %s\n", t.label(), t.path)
			continue
		case !install && cur == "":
			fmt.Fprintf(stdout, "%s: not installed in %s\n", t.label(), t.path)
			continue
		}
		_, out, err := editHooks(old, install, cmd)
		if err != nil {
			return fmt.Errorf("%s: %s: %w (left unchanged)", t.label(), t.path, err)
		}
		verb := "installed"
		if !install {
			verb = "removed"
		} else if cur != "" {
			verb = "updated"
		}
		if hookDryRun {
			fmt.Fprintf(stdout, "%s: would be %s in %s:\n%s", t.label(), verb, t.path, out)
			continue
		}
		if err := writeHookFile(t.path, old, out); err != nil {
			return err
		}
		note := ""
		if len(old) > 0 {
			note = " (previous file: " + t.path + ".oko.bak)"
		}
		fmt.Fprintf(stdout, "%s: %s in %s%s\n", t.label(), verb, t.path, note)
		if install && t.agent == "codex" {
			fmt.Fprintln(stdout, "  Codex runs new hooks only once trusted: open /hooks in Codex and trust it.")
		}
		if install && t.agent == "claude" {
			fmt.Fprintln(stdout, "  Takes effect in new Claude Code sessions (or after /hooks in a running one).")
		}
	}
	return nil
}

var hookCmd = &cobra.Command{
	Use:   "hook [install|uninstall|status]",
	Short: "Put 'oko picks --hook' into Claude Code / Codex, so browser picks join your next message",
	Long: "Installs the UserPromptSubmit hook that hands the agent whatever you picked in\n" +
		"the browser (Alt+P) since your last message. Default: user-wide, for every agent\n" +
		"whose config dir exists ($CLAUDE_CONFIG_DIR or ~/.claude/settings.json,\n" +
		"$CODEX_HOME or ~/.codex/hooks.json). Other settings and hooks are kept; the\n" +
		"previous file is saved as <file>.oko.bak. Running it again changes nothing.",
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"install", "uninstall", "status"},
	RunE: func(cmd *cobra.Command, args []string) error {
		what := "status"
		if len(args) == 1 {
			what = args[0]
		}
		switch what {
		case "install":
			return hookEdit(true)
		case "uninstall", "remove":
			return hookEdit(false)
		case "status":
			targets, err := hookTargets()
			if err != nil {
				return err
			}
			for _, t := range targets {
				b, _ := os.ReadFile(t.path)
				if c := installedCommand(b); c != "" {
					fmt.Fprintf(stdout, "%s: installed in %s (%s)\n", t.label(), t.path, c)
				} else {
					fmt.Fprintf(stdout, "%s: not installed (%s); 'oko hook install'\n", t.label(), t.path)
				}
			}
			return nil
		}
		return fmt.Errorf("unknown %q: use install, uninstall or status", what)
	},
}

func init() {
	f := hookCmd.Flags()
	f.StringVar(&hookAgents, "agent", "", "claude, codex or all (default: every agent whose config dir exists)")
	f.BoolVar(&hookProject, "project", false, "this project only: ./.claude/settings.json, ./.codex/hooks.json")
	f.StringVar(&hookFile, "file", "", "edit this settings/hooks file instead (with one --agent)")
	f.BoolVar(&hookDryRun, "dry-run", false, "print the resulting file instead of writing it")
	rootCmd.AddCommand(hookCmd)
}
