package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func settingsFixture(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if body == "" {
		return home
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func settingsBody(t *testing.T, home string) map[string]json.RawMessage {
	t.Helper()
	m, err := readSettings(SettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestEnsureModelSetting_WritesOnlyWhenNothingIsRecorded: the default is a
// default. An operator who picked a model has decided where their turns run,
// and waired-agent#1037 is the change that makes that decision mean something
// — overwriting it would be the same class of defect from the other side.
func TestEnsureModelSetting_WritesOnlyWhenNothingIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantKind ModelSettingKind
		wantWrit string
		wantFile string
	}{
		{"no settings file at all", "", ModelSettingNone, DirectiveModelAuto, DirectiveModelAuto},
		{"a settings file with no model", `{"theme":"dark"}`, ModelSettingNone, DirectiveModelAuto, DirectiveModelAuto},
		{"the operator picked a real model", `{"model":"claude-fable-5[1m]"}`, ModelSettingForeign, "", "claude-fable-5[1m]"},
		{"the operator picked a Waired row", `{"model":"anthropic-waired-local"}`, ModelSettingOurs, "", "anthropic-waired-local"},
		{"already ours", `{"model":"claude-waired-auto"}`, ModelSettingOurs, "", "claude-waired-auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := settingsFixture(t, tc.body)
			res, err := EnsureModelSetting(home)
			if err != nil {
				t.Fatal(err)
			}
			if res.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", res.Kind, tc.wantKind)
			}
			if res.Wrote != tc.wantWrit {
				t.Errorf("Wrote = %q, want %q", res.Wrote, tc.wantWrit)
			}
			var got string
			if raw, ok := settingsBody(t, home)[modelSettingKey]; ok {
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
			}
			if got != tc.wantFile {
				t.Errorf("settings model = %q, want %q", got, tc.wantFile)
			}
		})
	}
}

// TestEnsureModelSetting_LeavesTheRestOfTheFileAlone: this is the user's own
// file, shared with the statusLine key and everything Claude Code writes there.
func TestEnsureModelSetting_LeavesTheRestOfTheFileAlone(t *testing.T) {
	home := settingsFixture(t, `{"theme":"dark","statusLine":{"type":"command","command":"waired claude statusline"}}`)
	if _, err := EnsureModelSetting(home); err != nil {
		t.Fatal(err)
	}
	m := settingsBody(t, home)
	for _, key := range []string{"theme", "statusLine", modelSettingKey} {
		if _, ok := m[key]; !ok {
			t.Errorf("key %q missing after the write: %v", key, m)
		}
	}
}

// TestRemoveModelSetting: only ours goes, and the file goes with it when that
// was the last key — whichever of this and RemoveStatusLine runs second is the
// one that finds the file empty.
func TestRemoveModelSetting(t *testing.T) {
	t.Run("drops ours", func(t *testing.T) {
		home := settingsFixture(t, `{"model":"claude-waired-auto","theme":"dark"}`)
		if err := RemoveModelSetting(home); err != nil {
			t.Fatal(err)
		}
		if _, ok := settingsBody(t, home)[modelSettingKey]; ok {
			t.Error("ours survived the remove")
		}
		if _, ok := settingsBody(t, home)["theme"]; !ok {
			t.Error("the remove took the rest of the file with it")
		}
	})

	t.Run("leaves the operator's own choice", func(t *testing.T) {
		home := settingsFixture(t, `{"model":"claude-fable-5"}`)
		if err := RemoveModelSetting(home); err != nil {
			t.Fatal(err)
		}
		var got string
		raw, ok := settingsBody(t, home)[modelSettingKey]
		if !ok {
			t.Fatal("the operator's model was removed")
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got != "claude-fable-5" {
			t.Errorf("model = %q, want it untouched", got)
		}
	})

	t.Run("removes a file that held nothing else", func(t *testing.T) {
		home := settingsFixture(t, `{"model":"claude-waired-auto"}`)
		if err := RemoveModelSetting(home); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(SettingsPath(home)); !os.IsNotExist(err) {
			t.Errorf("an empty settings file was left behind (err=%v)", err)
		}
	})

	t.Run("nothing recorded is not an error", func(t *testing.T) {
		home := settingsFixture(t, "")
		if err := RemoveModelSetting(home); err != nil {
			t.Errorf("RemoveModelSetting on a host with no settings: %v", err)
		}
	})
}
