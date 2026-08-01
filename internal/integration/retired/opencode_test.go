package retired_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/retired"
)

// seedOpenCodeInstall lays down what the deleted adapter used to write:
// the plugin, the two affordance commands, and the ledger entry naming
// them. Returns (home, stateDir).
func seedOpenCodeInstall(t *testing.T, withLedger bool) (string, string) {
	t.Helper()
	home := t.TempDir()
	stateDir := filepath.Join(home, ".config", "waired")

	pluginDir := filepath.Join(home, ".config", "opencode", "plugin")
	cmdDir := filepath.Join(home, ".config", "opencode", "commands")
	for _, d := range []string{pluginDir, cmdDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("// waired\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	write(filepath.Join(pluginDir, "waired.js"))
	cmdFiles := []string{
		filepath.Join(cmdDir, "waired-status.md"),
		filepath.Join(cmdDir, "waired-doctor.md"),
	}
	for _, f := range cmdFiles {
		write(f)
	}

	if withLedger {
		paths, err := integration.PathsFor(stateDir)
		if err != nil {
			t.Fatalf("PathsFor: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(paths.Ledger), 0o755); err != nil {
			t.Fatalf("mkdir ledger dir: %v", err)
		}
		l, err := integration.LoadLedger(paths.Ledger)
		if err != nil {
			t.Fatalf("LoadLedger: %v", err)
		}
		l.Set(retired.OpenCodeAgentID, integration.AgentRecord{
			SkillFiles: cmdFiles,
			ConfigPath: filepath.Join(pluginDir, "waired.js"),
			OwnedFully: true,
		})
		if err := l.Save(paths.Ledger); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	return home, stateDir
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("still present after the sweep: %s (err=%v)", p, err)
		}
	}
}

// The contract this package exists for: an install that already carries
// the OpenCode artifacts must still be scrubbable after the adapter is
// gone. Product contract — if this breaks, every host that ever ran the
// integration keeps its files forever, including through a full Waired
// uninstall.
func TestSweepOpenCode_RemovesAdapterArtifactsAndLedgerEntry(t *testing.T) {
	home, stateDir := seedOpenCodeInstall(t, true)

	if err := retired.SweepOpenCode(home, stateDir); err != nil {
		t.Fatalf("SweepOpenCode: %v", err)
	}

	oc := filepath.Join(home, ".config", "opencode")
	assertGone(t,
		filepath.Join(oc, "plugin", "waired.js"),
		filepath.Join(oc, "commands", "waired-status.md"),
		filepath.Join(oc, "commands", "waired-doctor.md"),
		// Both directories were created by the adapter and are now empty.
		filepath.Join(oc, "plugin"),
		filepath.Join(oc, "commands"),
	)

	paths, err := integration.PathsFor(stateDir)
	if err != nil {
		t.Fatalf("PathsFor: %v", err)
	}
	l, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	if _, ok := l.Get(retired.OpenCodeAgentID); ok {
		t.Error("ledger still records the opencode adapter after the sweep")
	}
}

// The ledger is advisory, not required. An install whose state dir was
// lost (or that predates the SkillFiles record) still has the files on
// disk, and the canonical names have to reach them.
func TestSweepOpenCode_WithoutLedgerStillRemovesTheFiles(t *testing.T) {
	home, _ := seedOpenCodeInstall(t, false)

	if err := retired.SweepOpenCode(home, ""); err != nil {
		t.Fatalf("SweepOpenCode: %v", err)
	}

	oc := filepath.Join(home, ".config", "opencode")
	assertGone(t,
		filepath.Join(oc, "plugin", "waired.js"),
		filepath.Join(oc, "commands", "waired-status.md"),
		filepath.Join(oc, "commands", "waired-doctor.md"),
	)
}

// Waired only ever wrote INTO OpenCode's directories. A user's own
// plugin or command keeps its directory alive, and nothing of theirs is
// touched.
func TestSweepOpenCode_LeavesForeignFilesAndTheirDirectories(t *testing.T) {
	home, stateDir := seedOpenCodeInstall(t, true)
	oc := filepath.Join(home, ".config", "opencode")
	mine := filepath.Join(oc, "plugin", "my-own.js")
	if err := os.WriteFile(mine, []byte("// mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := retired.SweepOpenCode(home, stateDir); err != nil {
		t.Fatalf("SweepOpenCode: %v", err)
	}

	if _, err := os.Stat(mine); err != nil {
		t.Errorf("a user's own plugin was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oc, "plugin")); err != nil {
		t.Errorf("plugin/ was removed while it still held a user file: %v", err)
	}
	// OpenCode's own config dir is never ours to delete.
	if _, err := os.Stat(oc); err != nil {
		t.Errorf("~/.config/opencode was removed: %v", err)
	}
}

// A host that never ran the integration must not be reported as a failed
// uninstall — `waired unlink` surfaces the first non-nil error it gets.
func TestSweepOpenCode_NothingInstalledIsNotAnError(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".config", "waired")
	if err := retired.SweepOpenCode(home, stateDir); err != nil {
		t.Fatalf("SweepOpenCode on a clean host: %v", err)
	}
	if err := retired.SweepOpenCode(home, stateDir); err != nil {
		t.Fatalf("SweepOpenCode is not idempotent: %v", err)
	}
}
