package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookInstallKeepsSettings(t *testing.T) {
	f := filepath.Join(t.TempDir(), "settings.json")
	orig := `{"model":"opus","hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"other-tool"}]}],"Stop":[]},"zeta":1}`
	if err := os.WriteFile(f, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	must(t, oko(t, "hook", "install", "--agent", "claude", "--file", f), "installed in")
	b, _ := os.ReadFile(f)
	s := string(b)
	must(t, s, `"command": "other-tool"`, "picks --hook", `"Stop": []`, `"zeta": 1`)
	if strings.Index(s, `"model"`) > strings.Index(s, `"hooks"`) || strings.Index(s, `"hooks"`) > strings.Index(s, `"zeta"`) {
		t.Fatalf("key order changed:\n%s", s)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode().Perm())
	}
	bak, _ := os.ReadFile(f + ".oko.bak")
	if string(bak) != orig {
		t.Fatalf("backup differs:\n%s", bak)
	}

	must(t, oko(t, "hook", "install", "--agent", "claude", "--file", f), "already installed")
	if n := strings.Count(mustRead(t, f), "picks --hook"); n != 1 {
		t.Fatalf("%d oko hooks after a second install", n)
	}
	must(t, oko(t, "hook", "status", "--agent", "claude", "--file", f), "installed in")

	must(t, oko(t, "hook", "uninstall", "--agent", "claude", "--file", f), "removed in")
	s = mustRead(t, f)
	mustNot(t, s, "picks --hook")
	must(t, s, "other-tool", `"zeta": 1`)

	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("not json"), 0o600)
	must(t, oko(t, "!", "hook", "install", "--agent", "claude", "--file", bad), "left unchanged")
	must(t, mustRead(t, bad), "not json")
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
