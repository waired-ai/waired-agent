// Package retired removes the on-disk traces of integrations Waired
// used to support and no longer does.
//
// It exists because uninstall is not symmetric with install. Deleting an
// adapter deletes the only code that knew how to take its artifacts back
// off a user's machine: `waired unlink` walks the REGISTERED adapters
// (internal/integration.Manager.UninstallAll), so an integration removed
// from that list leaves whatever it wrote behind forever — on every host
// that ever ran it, including hosts that later uninstall Waired
// entirely. The install/uninstall scripts do not fill the gap; they only
// shell out to `waired unlink`.
//
// So a withdrawn integration keeps exactly one thing: the ability to
// undo itself. Nothing here implements integration.Adapter, on purpose —
// an Adapter is something the manager can also Apply, Detect and Audit,
// and it would come back in `waired doctor` output, in the `waired init`
// consent prompt and in the setup wizard's target list the moment
// somebody added it to a registration site by reflex. A plain function
// with one caller cannot.
//
// Entries here are temporary by design. Remove one once the release that
// dropped the integration is far enough behind that no supported install
// can still be carrying its files.
package retired

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// OpenCodeAgentID is the ledger key the deleted OpenCode adapter wrote
// under. It matches the retired proto wire value
// (signer.IntegrationOpenCode) and the adapter's own former
// integration.AgentID; the literal is repeated rather than referenced
// because both of those are meant to shrink away and this sweep must
// keep working when they do.
const OpenCodeAgentID integration.AgentID = "opencode"

// openCodeCommandFiles are the affordance command files the adapter
// installed. Named literally because the templates they were rendered
// from are gone: the ledger is the primary source and this list is only
// the fallback for an install whose ledger was lost or predates the
// SkillFiles record.
var openCodeCommandFiles = []string{"waired-status", "waired-doctor"}

// SweepOpenCode removes everything the OpenCode adapter ever wrote into
// homeDir and drops its ledger entry (waired-agent#333).
//
// Best-effort by the same rule the adapters use: an artifact that is
// already gone is not an error, and a directory Waired created is
// removed only when it is left empty, so a user's own plugins and
// commands survive.
//
// Returns nil when there is nothing to do, including when the ledger
// never mentioned OpenCode — a host that never ran the integration must
// not be made to look like a failed uninstall.
func SweepOpenCode(homeDir, stateDir string) error {
	if homeDir == "" {
		return fmt.Errorf("retired: opencode sweep: empty HomeDir")
	}
	if err := removeOpenCodePlugin(homeDir); err != nil {
		return err
	}

	// The ledger is advisory here. Its SkillFiles are the exact paths the
	// adapter created, but an install can be missing them (a ledger lost
	// with the state dir, or one written before the record existed) while
	// the files are still on disk — so the canonical names are swept too,
	// not instead.
	var recorded []string
	var ledger *integration.Ledger
	var ledgerPath string
	if stateDir != "" {
		paths, err := integration.PathsFor(stateDir)
		if err != nil {
			return err
		}
		ledgerPath = paths.Ledger
		if ledger, err = integration.LoadLedger(ledgerPath); err != nil {
			return err
		}
		if rec, ok := ledger.Get(OpenCodeAgentID); ok {
			recorded = rec.SkillFiles
		}
	}

	files := append([]string{}, recorded...)
	for _, name := range openCodeCommandFiles {
		files = append(files, openCodeCommandFile(homeDir, name))
	}
	if err := removeOpenCodeCommands(homeDir, files); err != nil {
		return err
	}

	if ledger == nil {
		return nil
	}
	if _, ok := ledger.Get(OpenCodeAgentID); !ok {
		// Nothing recorded: leave the ledger byte-identical rather than
		// rewriting it (and bumping UpdatedAt) on every unlink forever.
		return nil
	}
	ledger.Delete(OpenCodeAgentID)
	return ledger.Save(ledgerPath)
}

// openCodeConfigDir is ~/.config/opencode — OpenCode's own directory,
// which Waired only ever wrote INTO. It is never removed here.
func openCodeConfigDir(home string) string { return filepath.Join(home, ".config", "opencode") }

func openCodePluginDir(home string) string { return filepath.Join(openCodeConfigDir(home), "plugin") }
func openCodePluginFile(home string) string {
	return filepath.Join(openCodePluginDir(home), "waired.js")
}
func openCodeCommandsDir(home string) string {
	return filepath.Join(openCodeConfigDir(home), "commands")
}
func openCodeCommandFile(home, name string) string {
	return filepath.Join(openCodeCommandsDir(home), name+".md")
}

func removeOpenCodePlugin(home string) error {
	dst := openCodePluginFile(home)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("retired: opencode: remove %s: %w", dst, err)
	}
	removeIfEmpty(openCodePluginDir(home))
	return nil
}

func removeOpenCodeCommands(home string, files []string) error {
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("retired: opencode: remove %s: %w", f, err)
		}
	}
	removeIfEmpty(openCodeCommandsDir(home))
	return nil
}

// removeIfEmpty drops a directory Waired created, but only while it holds
// nothing — the user's own files in the same directory are not ours to
// take.
func removeIfEmpty(dir string) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}
