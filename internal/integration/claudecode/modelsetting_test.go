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

// waired no longer writes a default model into the user's settings (owner
// ruling 2026-09-03): with a turn that fails closed
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md),
// a Waired default on a computer with no engine and no peer would fail every
// session from the moment routing was enabled. Only the removal is left, and
// DetectModelSetting still classifies what is there so `waired claude status`
// can say which side new sessions start on.

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
