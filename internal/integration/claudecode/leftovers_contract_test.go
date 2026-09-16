package claudecode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUninstallScriptsKnowEveryUserOwnershipLiteral is the per-user half of
// claudemanaged's TestUninstallScriptsKnowEveryManagedOwnershipLiteral: the
// uninstall scripts carry a copy of disable's per-user rules for a host whose
// binary can't run them (waired-agent#1398), and every literal those rules
// recognise must appear in both scripts, so a new one fails here until the
// scripts learn it. The corpus under
// packaging/install/testdata/claude-leftovers is what holds each copy to the
// behaviour.
//
// PIN: record of today's behaviour -- the list is what this package's
// removers recognise on 2026-09-16.
func TestUninstallScriptsKnowEveryUserOwnershipLiteral(t *testing.T) {
	literals := []string{
		statuslineKey, statuslineMarker, statuslineStashKey, statuslineWrapperStem, statuslineOrigStore,
		modelPickerKey, modelSettingKey,
		subagentModelEnvKey, subagentForceEnvKey,
		RouteSkillName, retiredCacheFile, retiredCacheLoopbackPrefix, claudeConfigDirEnv,
		wairedIDMarker, TierMarker1M,
		fallbackMarkerDir,
	}
	for name := range skillFiles {
		literals = append(literals, name)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	for _, script := range []string{"packaging/install/uninstall.ps1", "packaging/install/uninstall.sh"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(script)))
		if err != nil {
			t.Fatal(err)
		}
		for _, lit := range literals {
			if !strings.Contains(string(b), lit) {
				t.Errorf("%s doesn't know %q, which the per-user removers recognise (waired-agent#1398)", script, lit)
			}
		}
	}
}
