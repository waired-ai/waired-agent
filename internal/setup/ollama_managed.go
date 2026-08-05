package setup

import (
	"os"
	"path/filepath"
)

// WairedManagedMarkerName is the marker file the waired installer helpers
// write next to an Ollama install they created (scripts/install/ollama-windows.ps1;
// the Linux and macOS installers answer the same question by path instead). Its
// presence tells a waired-made install from the user's own, which matters
// because both live at the exact same well-known path (%ProgramFiles%\Ollama).
//
// Windows is the last OS that needs it. macOS used to record ownership in the
// state dir instead, because its engine was a signed .app bundle and adding any
// file to such a bundle's root invalidates its v2 resource seal — `codesign`
// then reports "unsealed contents present in the bundle root", `spctl` rejects,
// and on Apple Silicon every exec of the bundled binary is killed by
// Gatekeeper/AMFI (#329). #492 moved the macOS engine under the state dir,
// where there is no bundle and no shared location, so "is this install ours" is
// answered by the path — and the record, the bundle probe and the repair that
// existed for that layout went with it.
const WairedManagedMarkerName = ".waired-managed.json"

// ManagedFacts are the filesystem observations that decide whether an Ollama
// install was made by waired. Gathering them is separated from judging them so
// the judgement is an untagged (GOOS, facts) -> bool function that table-tests
// on every runner.
type ManagedFacts struct {
	// MarkerBesideBinary: <dir(binary)>/.waired-managed.json exists. The
	// Windows installer's completion receipt.
	MarkerBesideBinary bool
}

// managedFrom judges the facts per OS.
//
// Windows keeps the same-dir marker as its only signal: the install is a plain
// directory with no seal to break, and the Windows repair arm (engineIncomplete)
// reads the ABSENCE of the marker as "extracted but never configured".
//
// Linux and macOS answer by path — an engine either is the binary under the
// state dir or is not waired's — so no marker can say yes for them.
func managedFrom(goos string, f ManagedFacts) bool {
	return goos == "windows" && f.MarkerBesideBinary
}

// gatherManagedFacts observes the facts for the install at binPath.
func gatherManagedFacts(binPath string) ManagedFacts {
	return ManagedFacts{
		MarkerBesideBinary: fileExists(filepath.Join(filepath.Dir(binPath), WairedManagedMarkerName)),
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
