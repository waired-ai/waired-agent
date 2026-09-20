package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSettingsPathFollowsTheConfigDirectory.
//
// PIN: product contract — waired writes the settings file Claude Code reads.
// CLAUDE_CONFIG_DIR relocates the whole config directory, settings.json
// included, and the two locations do not merge: measured on 2.1.278,
// 2026-09-20, a lineup left in ~/.claude/settings.json with the variable set
// produced no Waired rows at all (waired-agent#1457).
func TestSettingsPathFollowsTheConfigDirectory(t *testing.T) {
	home := filepath.Join("/tmp", "home")
	elsewhere := filepath.Join("/tmp", "elsewhere")

	t.Run("the default location when nothing moved it", func(t *testing.T) {
		if got, want := SettingsPathFor("", home), filepath.Join(home, ".claude", "settings.json"); got != want {
			t.Errorf("SettingsPathFor(\"\", %q) = %q, want %q", home, got, want)
		}
	})
	t.Run("the config directory when one is named", func(t *testing.T) {
		if got, want := SettingsPathFor(elsewhere, home), filepath.Join(elsewhere, "settings.json"); got != want {
			t.Errorf("SettingsPathFor(%q, %q) = %q, want %q", elsewhere, home, got, want)
		}
	})
	t.Run("SettingsPath reads the environment", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", elsewhere)
		if got, want := SettingsPath(home), SettingsPathFor(elsewhere, home); got != want {
			t.Errorf("SettingsPath(%q) = %q, want %q", home, got, want)
		}
	})
	t.Run("and falls back when it is unset", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		if got, want := SettingsPath(home), SettingsPathFor("", home); got != want {
			t.Errorf("SettingsPath(%q) = %q, want %q", home, got, want)
		}
	})
}

// TestEverySurfaceWritesUnderTheConfigDirectory: the path function is shared,
// so this is about the surfaces actually going through it. Before
// waired-agent#1457 all four wrote ~/.claude/settings.json and reported
// success while Claude Code read nothing.
func TestEverySurfaceWritesUnderTheConfigDirectory(t *testing.T) {
	home := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	live := SettingsPathFor(configDir, home)
	stranded := SettingsPathFor("", home)

	if _, err := WritePickerLineup(SettingsPath(home), []PickerRow{{Model: "waired"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetSubagentPlacement(SettingsPath(home), SubagentWaired, "waired"); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallStatusLine(home, false); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("nothing was written where Claude Code reads: %v", err)
	}
	for _, want := range []string{"modelPicker", "CLAUDE_CODE_SUBAGENT_MODEL", "statusLine"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("%s is missing from %s: %s", want, live, body)
		}
	}
	if _, err := os.Stat(stranded); !os.IsNotExist(err) {
		t.Errorf("%s was written as well; the config directory is the only place Claude Code reads", stranded)
	}
}

// TestRemoveWairedSettingsAtClearsTheStrandedTwin: `waired claude disable`
// sweeps the file this run is not writing, so keys left there by a run made
// with a different CLAUDE_CONFIG_DIR do not come back live when the variable
// changes again. Anything that is not waired's is left where it is, in the
// twin exactly as in the live file.
func TestRemoveWairedSettingsAtClearsTheStrandedTwin(t *testing.T) {
	t.Run("takes all four of waired's keys and then the file", func(t *testing.T) {
		home := t.TempDir()
		path := SettingsPathFor("", home)
		if _, err := WritePickerLineup(path, []PickerRow{{Model: "waired"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := SetSubagentPlacement(path, SubagentWaired, "waired"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(SettingsPathFor("", home), mergeInto(t, path, `"model":"waired/local"`), 0o600); err != nil {
			t.Fatal(err)
		}

		removed, err := RemoveWairedSettingsAt(path)
		if err != nil || !removed {
			t.Fatalf("RemoveWairedSettingsAt = (%v, %v), want (true, nil)", removed, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			b, _ := os.ReadFile(path)
			t.Errorf("the emptied file was left behind: %s", b)
		}
	})

	t.Run("leaves what is not ours, and the file with it", func(t *testing.T) {
		home := t.TempDir()
		path := SettingsPathFor("", home)
		body := `{"modelPicker":{"options":[{"model":"us.anthropic.claude-opus-4-8"}]},` +
			`"model":"opus","statusLine":{"type":"command","command":"theirs"}}`
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		removed, err := RemoveWairedSettingsAt(path)
		if err != nil {
			t.Fatalf("RemoveWairedSettingsAt errored: %v", err)
		}
		if removed {
			t.Error("RemoveWairedSettingsAt = true, want false")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the operator's file was removed: %v", err)
		}
		if string(after) != body {
			t.Errorf("the operator's file was rewritten\n got: %s\nwant: %s", after, body)
		}
	})
}

// mergeInto adds one raw JSON member to an existing settings file's object and
// returns the bytes, so a test can seed a key no exported helper writes.
func mergeInto(t *testing.T, path, member string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(b))
	if !strings.HasPrefix(trimmed, "{") {
		t.Fatalf("%s is not a JSON object: %s", path, b)
	}
	return []byte("{" + member + "," + trimmed[1:])
}
