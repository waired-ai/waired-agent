package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// The #599 blind spot: waired only read ~/.claude/settings.json, so a
// statusLine set in a higher-precedence scope (managed, project
// .claude/settings.local.json, project .claude/settings.json) silently
// shadowed the injected segment while enable/status reported success.
// DetectEffectiveStatusLine walks the documented precedence for a session
// rooted at cwd.

func writeSettingsFixture(t *testing.T, path, command string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"statusLine":{"type":"command","command":` + jsonQuote(command) + `}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonQuote(s string) string {
	// good enough for fixture commands without quotes/backslashes
	return `"` + s + `"`
}

func TestDetectEffectiveStatusLine(t *testing.T) {
	newDirs := func(t *testing.T) (home, project, nested, managed string) {
		t.Helper()
		root := t.TempDir()
		home = filepath.Join(root, "home")
		project = filepath.Join(root, "src", "proj")
		nested = filepath.Join(project, "sub", "dir")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		managed = filepath.Join(root, "etc", "managed-settings.json")
		return
	}

	t.Run("nothing set anywhere", func(t *testing.T) {
		home, _, nested, managed := newDirs(t)
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeNone {
			t.Errorf("Scope = %q, want none; got %+v", eff.Scope, eff)
		}
	})

	t.Run("user scope only", func(t *testing.T) {
		home, _, nested, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), "bash ~/.claude/statusline.sh")
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeUser || eff.Kind != StatusLineForeign {
			t.Errorf("got %+v, want user/foreign", eff)
		}
	})

	t.Run("project local shadows user", func(t *testing.T) {
		home, project, nested, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), statuslineRenderCommand)
		writeSettingsFixture(t, filepath.Join(project, ".claude", "settings.local.json"), "bash ~/.claude/statusline.sh")
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeProjectLocal || eff.Kind != StatusLineForeign {
			t.Errorf("got %+v, want project-local/foreign", eff)
		}
		if eff.Path != filepath.Join(project, ".claude", "settings.local.json") {
			t.Errorf("Path = %q", eff.Path)
		}
		if eff.Command != "bash ~/.claude/statusline.sh" {
			t.Errorf("Command = %q", eff.Command)
		}
	})

	t.Run("project shared shadows user, local wins over shared", func(t *testing.T) {
		home, project, nested, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), statuslineRenderCommand)
		writeSettingsFixture(t, filepath.Join(project, ".claude", "settings.json"), "proj-shared")
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeProject || eff.Command != "proj-shared" {
			t.Errorf("got %+v, want project/proj-shared", eff)
		}

		writeSettingsFixture(t, filepath.Join(project, ".claude", "settings.local.json"), "proj-local")
		eff, err = DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeProjectLocal || eff.Command != "proj-local" {
			t.Errorf("got %+v, want project-local/proj-local", eff)
		}
	})

	t.Run("nearest project dir wins over ancestor", func(t *testing.T) {
		home, project, nested, managed := newDirs(t)
		writeSettingsFixture(t, filepath.Join(project, ".claude", "settings.json"), "outer")
		writeSettingsFixture(t, filepath.Join(nested, ".claude", "settings.json"), "inner")
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Command != "inner" {
			t.Errorf("got %+v, want the nearest project's statusLine", eff)
		}
	})

	t.Run("managed wins over everything", func(t *testing.T) {
		home, project, nested, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), statuslineRenderCommand)
		writeSettingsFixture(t, filepath.Join(project, ".claude", "settings.local.json"), "proj-local")
		writeSettingsFixture(t, managed, "managed-cmd")
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeManaged || eff.Command != "managed-cmd" {
			t.Errorf("got %+v, want managed/managed-cmd", eff)
		}
	})

	t.Run("waired-owned user statusline is effective when nothing shadows", func(t *testing.T) {
		home, _, nested, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), statuslineRenderCommand)
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeUser || eff.Kind != StatusLineOurs {
			t.Errorf("got %+v, want user/ours", eff)
		}
	})

	t.Run("home .claude dir is not treated as a project scope", func(t *testing.T) {
		home, _, _, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), "bash ~/.claude/statusline.sh")
		// cwd inside home: walking up must not classify ~/.claude/settings.json
		// as a project file (that would double-count the user scope).
		cwd := filepath.Join(home, "work")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		eff, err := DetectEffectiveStatusLine(home, cwd, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeUser {
			t.Errorf("Scope = %q, want user (home is not a project)", eff.Scope)
		}
	})

	t.Run("empty cwd still reports user scope", func(t *testing.T) {
		home, _, _, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), "custom")
		eff, err := DetectEffectiveStatusLine(home, "", managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeUser || eff.Command != "custom" {
			t.Errorf("got %+v, want user/custom", eff)
		}
	})

	t.Run("unreadable project file is skipped best-effort", func(t *testing.T) {
		home, project, nested, managed := newDirs(t)
		writeSettingsFixture(t, SettingsPath(home), "user-cmd")
		p := filepath.Join(project, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		eff, err := DetectEffectiveStatusLine(home, nested, managed)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Scope != ScopeUser || eff.Command != "user-cmd" {
			t.Errorf("got %+v, want user scope after skipping the malformed file", eff)
		}
	})
}
