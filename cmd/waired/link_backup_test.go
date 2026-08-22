package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// writeBackupLedger writes a ledger naming a config backup for openclaw, the
// way Apply does after it changes a config the user already had.
func writeBackupLedger(t *testing.T, stateDir, backupPath string) {
	t.Helper()
	paths, err := integration.PathsFor(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Set(integration.AgentOpenClaw, integration.AgentRecord{
		AppliedAt:  time.Now().UTC(),
		BackupPath: backupPath,
	})
	if err := ledger.Save(paths.Ledger); err != nil {
		t.Fatal(err)
	}
}

// TestRecordedConfigBackups_NamesAnExistingBackup: unlink leaves the copy of
// the config taken before waired first changed it, so it has to say where it
// is — otherwise the file reads as residue nobody claims (#995).
func TestRecordedConfigBackups_NamesAnExistingBackup(t *testing.T) {
	stateDir := t.TempDir()
	bak := filepath.Join(t.TempDir(), "openclaw.json.waired-bak-1700000001")
	if err := os.WriteFile(bak, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBackupLedger(t, stateDir, bak)

	got := recordedConfigBackups(stateDir, allIntegrationIDs())
	if got[integration.AgentOpenClaw] != bak {
		t.Fatalf("recordedConfigBackups = %v, want openclaw -> %s", got, bak)
	}

	var buf bytes.Buffer
	printKeptConfigBackups(&buf, got)
	out := buf.String()
	if !strings.Contains(out, bak) {
		t.Errorf("output does not name the backup:\n%s", out)
	}
	if !strings.Contains(out, "openclaw") {
		t.Errorf("output does not say which config it belongs to:\n%s", out)
	}
}

// TestRecordedConfigBackups_SkipsAVanishedFile: the ledger can outlive the
// file if the user deleted it. Printing a path that is not there would be
// worse than saying nothing.
func TestRecordedConfigBackups_SkipsAVanishedFile(t *testing.T) {
	stateDir := t.TempDir()
	bak := filepath.Join(t.TempDir(), "openclaw.json.waired-bak-1700000001")
	writeBackupLedger(t, stateDir, bak) // never created on disk

	got := recordedConfigBackups(stateDir, allIntegrationIDs())
	if len(got) != 0 {
		t.Fatalf("recordedConfigBackups = %v, want empty", got)
	}
	var buf bytes.Buffer
	printKeptConfigBackups(&buf, got)
	if buf.Len() != 0 {
		t.Errorf("printed something for a backup that is not on disk:\n%s", buf.String())
	}
}

// TestRecordedConfigBackups_NoLedgerIsQuiet: an unreadable or absent ledger
// must not turn an otherwise clean removal into noise or a failure.
func TestRecordedConfigBackups_NoLedgerIsQuiet(t *testing.T) {
	got := recordedConfigBackups(t.TempDir(), allIntegrationIDs())
	if len(got) != 0 {
		t.Fatalf("recordedConfigBackups on an empty state dir = %v, want empty", got)
	}
}

// TestPrintLinkPlan_UninstallMentionsTheKeptBackup: `--dry-run` is where a
// user checks what removal will do. It lists everything unlink deletes, so
// the one thing it deliberately keeps belongs there too.
func TestPrintLinkPlan_UninstallMentionsTheKeptBackup(t *testing.T) {
	out := captureLinkStdout(t, func() {
		if err := printLinkPlan("all", true, false, "/home/u", "/var/lib/waired", "http://127.0.0.1:9473"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "waired-bak") {
		t.Errorf("removal plan does not mention the backup it keeps:\n%s", out)
	}
}
