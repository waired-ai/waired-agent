package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func writeVSCodeSettings(t *testing.T, home, flavorSubdir, body string) string {
	t.Helper()
	path := filepath.Join(home, ".config", flavorSubdir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVSCodeWrapper_NotInstalled(t *testing.T) {
	got := VSCodeWrapper(t.TempDir(), "/p/waired claude")
	if len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestVSCodeWrapper_PureJSON_Configured(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json",
		`{"editor.fontSize": 14, "claude.claudeProcessWrapper": "/p/waired claude"}`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if !got[0].Configured || got[0].Stale || got[0].CurrentValue != "/p/waired claude" {
		t.Errorf("%+v", got[0])
	}
}

func TestVSCodeWrapper_JSONC_LineComments(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json", `{
  // editor settings
  "editor.fontSize": 14, // pixels
  "claude.claudeProcessWrapper": "/p/waired claude" // managed by waired
}`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 1 || got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestVSCodeWrapper_JSONC_BlockCommentsAndTrailingComma(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json", `{
  /* block
     comment */
  "claude.claudeProcessWrapper": "/p/waired claude",
}`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 1 || got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestVSCodeWrapper_StringContainingSlashSlash(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json", `{
  "url": "https://example.com",
  "claude.claudeProcessWrapper": "/p/waired claude"
}`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 1 || got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestVSCodeWrapper_StalePath(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json",
		`{"claude.claudeProcessWrapper": "/old/waired claude"}`)
	got := VSCodeWrapper(home, "/new/waired claude")
	if len(got) != 1 || !got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestVSCodeWrapper_NoKey(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json", `{"editor.fontSize": 14}`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 1 || got[0].Configured {
		t.Fatalf("%+v", got)
	}
}

func TestVSCodeWrapper_Unparseable(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json", `{not json`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 1 || !got[0].Configured || !got[0].Stale {
		t.Fatalf("%+v", got)
	}
}

func TestVSCodeWrapper_MultipleFlavors(t *testing.T) {
	home := t.TempDir()
	writeVSCodeSettings(t, home, "Code/User/settings.json",
		`{"claude.claudeProcessWrapper": "/p/waired claude"}`)
	writeVSCodeSettings(t, home, "VSCodium/User/settings.json",
		`{"claude.claudeProcessWrapper": "/old/waired claude"}`)
	got := VSCodeWrapper(home, "/p/waired claude")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	flavors := map[string]bool{}
	for _, r := range got {
		flavors[r.Flavor] = r.Stale
	}
	if flavors["Code"] {
		t.Error("Code should not be stale")
	}
	if !flavors["VSCodium"] {
		t.Error("VSCodium should be stale")
	}
}
