package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// The /waired-route slash command is retired: it switched a machine-wide
// route between auto / waired / anthropic, and there is no route left to
// switch (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313). Nothing installs it; what has to keep
// working is taking one an earlier build left behind, on enable as well as on
// disable, so an upgraded host loses a command that does nothing.

func TestRemoveRouteSkill_TakesTheFileAndItsDirectory(t *testing.T) {
	home := t.TempDir()
	dir := SkillDir(home, RouteSkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SkillFile(home, RouteSkillName), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveRouteSkill(home); err != nil {
		t.Fatalf("RemoveRouteSkill: %v", err)
	}
	if _, err := os.Stat(SkillFile(home, RouteSkillName)); !os.IsNotExist(err) {
		t.Errorf("skill file should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("empty skill dir should be removed, stat err=%v", err)
	}
}

func TestRemoveRouteSkill_IsFineWithNothingThere(t *testing.T) {
	if err := RemoveRouteSkill(t.TempDir()); err != nil {
		t.Errorf("removing an absent skill should be a no-op: %v", err)
	}
}

// A user's own file under our directory name is not ours to delete: the
// directory only goes when removing the skill emptied it.
func TestRemoveRouteSkill_KeepsAUserFileBesideIt(t *testing.T) {
	home := t.TempDir()
	dir := SkillDir(home, RouteSkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(mine, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRouteSkill(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("a file we did not write was removed with the directory: %v", err)
	}
}

func TestRemoveRouteSkill_RejectsAnEmptyHome(t *testing.T) {
	if err := RemoveRouteSkill(""); err == nil {
		t.Error("an empty home must be an error, not a delete under the process cwd")
	}
}
