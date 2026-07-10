package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJB(t *testing.T, home, ideVer, fileName, body string) string {
	t.Helper()
	path := filepath.Join(home, ".config", "JetBrains", ideVer, "options", fileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJetBrainsWrapper_NotInstalled(t *testing.T) {
	got := JetBrainsWrapper(t.TempDir(), "/p/waired claude")
	if len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestJetBrainsWrapper_XMLForm_OK(t *testing.T) {
	home := t.TempDir()
	writeJB(t, home, "IntelliJIdea2025.1", "claude.xml", `<application>
  <component name="ClaudeCodeSettings">
    <option name="claudeCommand" value="/p/waired claude"/>
  </component>
</application>`)
	got := JetBrainsWrapper(home, "/p/waired claude")
	if len(got) != 1 || got[0].Stale {
		t.Fatalf("%+v", got)
	}
	if got[0].Flavor != "IntelliJIdea2025.1" {
		t.Errorf("Flavor = %q", got[0].Flavor)
	}
}

func TestJetBrainsWrapper_SettingForm_Stale(t *testing.T) {
	home := t.TempDir()
	writeJB(t, home, "GoLand2025.2", "claude-code.xml", `<application>
  <component name="ClaudeCodeSettings">
    <setting name="claudeCode.claudeCommand" value="/old/waired claude"/>
  </component>
</application>`)
	got := JetBrainsWrapper(home, "/new/waired claude")
	if len(got) != 1 || !got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestJetBrainsWrapper_PropertiesForm(t *testing.T) {
	home := t.TempDir()
	writeJB(t, home, "PyCharm2025.1", "claude.properties",
		"claudeCode.claudeCommand=/p/waired claude\n")
	got := JetBrainsWrapper(home, "/p/waired claude")
	if len(got) != 1 || got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestJetBrainsWrapper_MultipleIDEs(t *testing.T) {
	home := t.TempDir()
	writeJB(t, home, "IntelliJIdea2025.1", "claude.xml",
		`<option name="claudeCommand" value="/p/waired claude"/>`)
	writeJB(t, home, "GoLand2025.2", "claude.xml",
		`<option name="claudeCommand" value="/old/waired claude"/>`)
	got := JetBrainsWrapper(home, "/p/waired claude")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
}

func TestJetBrainsWrapper_IgnoresUnrelatedFiles(t *testing.T) {
	home := t.TempDir()
	writeJB(t, home, "IntelliJIdea2025.1", "other.xml",
		`<option name="something" value="x"/>`)
	got := JetBrainsWrapper(home, "/p/waired claude")
	if len(got) != 0 {
		t.Fatalf("expected no results: %+v", got)
	}
}

func TestJetBrainsWrapper_XMLEntityDecode(t *testing.T) {
	home := t.TempDir()
	writeJB(t, home, "IntelliJIdea2025.1", "claude.xml",
		`<option name="claudeCommand" value="/p/waired&amp;tools claude"/>`)
	got := JetBrainsWrapper(home, "/p/waired&tools claude")
	if len(got) != 1 || got[0].Stale {
		t.Fatalf("%+v", got)
	}
}
