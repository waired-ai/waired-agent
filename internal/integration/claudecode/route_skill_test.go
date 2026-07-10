package claudecode

import (
	"os"
	"strings"
	"testing"
)

func TestInstallRouteSkillWritesFile(t *testing.T) {
	home := t.TempDir()
	if err := InstallRouteSkill(home); err != nil {
		t.Fatalf("InstallRouteSkill: %v", err)
	}
	dst := SkillFile(home, RouteSkillName)
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("skill file not written: %v", err)
	}
	s := string(body)
	// The frontmatter + `!` invocation are what make the slash command work.
	for _, want := range []string{
		"name: waired-route",
		"allowed-tools: Bash(waired claude route:*)",
		"disable-model-invocation: true",
		"!`waired claude route $ARGUMENTS`",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("skill body missing %q\n---\n%s", want, s)
		}
	}
}

func TestInstallRouteSkillIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := InstallRouteSkill(home); err != nil {
		t.Fatal(err)
	}
	if err := InstallRouteSkill(home); err != nil {
		t.Fatalf("second install should be idempotent: %v", err)
	}
	if _, err := os.Stat(SkillFile(home, RouteSkillName)); err != nil {
		t.Fatalf("skill missing after re-install: %v", err)
	}
}

func TestRemoveRouteSkill(t *testing.T) {
	home := t.TempDir()
	if err := InstallRouteSkill(home); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRouteSkill(home); err != nil {
		t.Fatalf("RemoveRouteSkill: %v", err)
	}
	if _, err := os.Stat(SkillFile(home, RouteSkillName)); !os.IsNotExist(err) {
		t.Errorf("skill file should be gone, stat err=%v", err)
	}
	// The now-empty skill dir should be cleaned up too.
	if _, err := os.Stat(SkillDir(home, RouteSkillName)); !os.IsNotExist(err) {
		t.Errorf("empty skill dir should be removed, stat err=%v", err)
	}
}

func TestRemoveRouteSkillMissingIsNoError(t *testing.T) {
	if err := RemoveRouteSkill(t.TempDir()); err != nil {
		t.Errorf("removing an absent skill should be a no-op, got %v", err)
	}
}

// The route skill must NOT be part of the init-time integration set — it is
// owned by `waired claude enable/disable`, not `waired init`.
func TestRouteSkillNotInInstalledSkills(t *testing.T) {
	for _, e := range installedSkills() {
		if e.Name == RouteSkillName {
			t.Fatalf("%s must not be in installedSkills() (owned by claude enable/disable)", RouteSkillName)
		}
	}
}
