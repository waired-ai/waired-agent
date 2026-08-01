package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WairedManagedMarkerName is the marker file the waired installer helpers
// write next to an Ollama install they created (scripts/install/ollama-windows.ps1;
// the Linux installer answers the same question by path instead). Its presence
// tells a waired-made install from the user's own, which matters because both
// live at the exact same well-known path (%ProgramFiles%\Ollama).
//
// It is deliberately NOT used on macOS any more: the macOS engine is a signed
// .app bundle, and adding any file to a signed bundle's root invalidates its v2
// resource seal — `codesign` then reports "unsealed contents present in the
// bundle root", `spctl` rejects, and on Apple Silicon every exec of the bundled
// binary is killed by Gatekeeper/AMFI. macOS records managed-ness in the state
// dir instead (see DarwinManagedRecordPath). (#329)
const WairedManagedMarkerName = ".waired-managed.json"

// darwinManagedRecordName is the state-dir record that replaces the in-bundle
// marker on macOS. It lives under the same <stateDir>/runtimes/<engine>/
// convention the bundled engine, its models and its logs already use.
const darwinManagedRecordName = "darwin-managed.json"

// DarwinManagedInstallerFresh / DarwinManagedInstallerRepair identify which
// path wrote the record, so a support log can tell a fresh install from a
// host that was migrated off the seal-breaking in-bundle marker.
const (
	DarwinManagedInstallerFresh  = "waired runtimes install ollama"
	DarwinManagedInstallerRepair = "waired repair (migrated from in-bundle marker)"
)

// DarwinManagedRecordPath is where macOS records that waired installed the
// Ollama.app at AppPath. Outside the bundle on purpose — see
// WairedManagedMarkerName.
func DarwinManagedRecordPath(stateDir string) string {
	return filepath.Join(stateDir, "runtimes", "ollama", darwinManagedRecordName)
}

// darwinManagedRecord is the on-disk shape of that record. AppPath is what
// makes it a statement about a specific install rather than a global flag: if
// the user replaces /Applications/Ollama.app with their own, the record no
// longer matches and detection stops claiming the install is ours.
type darwinManagedRecord struct {
	ManagedBy string `json:"managed_by"`
	Installer string `json:"installer"`
	AppPath   string `json:"app_path"`
}

// WriteDarwinManagedRecord records that waired owns the Ollama.app at appPath.
// Creating the parent is part of the job: on a fresh host the engine's state
// subtree does not exist until the daemon first spawns the engine.
func WriteDarwinManagedRecord(stateDir, appPath, installer string) error {
	if stateDir == "" {
		return errors.New("no state dir to record the waired-managed engine in")
	}
	dest := DarwinManagedRecordPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(darwinManagedRecord{
		ManagedBy: "waired",
		Installer: installer,
		AppPath:   appPath,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(dest, append(body, '\n'), 0o644)
}

// readDarwinManagedRecord returns the recorded app path, or "" when there is
// no readable record. Best-effort by design: a missing or corrupt record only
// means detection falls back to "not ours".
func readDarwinManagedRecord(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	body, err := os.ReadFile(DarwinManagedRecordPath(stateDir))
	if err != nil {
		return ""
	}
	var rec darwinManagedRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return ""
	}
	return rec.AppPath
}

// bundleRoot returns the nearest *.app ancestor of binPath, or "" when there
// is none. Pure path logic, no GOOS branch and no filesystem access: the macOS
// layout is Ollama.app/Contents/Resources/ollama, and a plain directory
// install simply walks to the root and reports "not a bundle".
func bundleRoot(binPath string) string {
	for d := filepath.Dir(binPath); ; {
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
		if strings.HasSuffix(strings.ToLower(d), ".app") {
			return d
		}
	}
}

// ManagedFacts are the three filesystem observations that decide whether an
// Ollama install was made by waired. Gathering them is separated from judging
// them so the judgement is an untagged (GOOS, facts) -> bool function that
// table-tests on every runner.
type ManagedFacts struct {
	// MarkerBesideBinary: <dir(binary)>/.waired-managed.json exists. The
	// Windows installer's completion receipt.
	MarkerBesideBinary bool
	// LegacyBundleMarker: <bundle root>/.waired-managed.json exists. This is
	// the seal-breaking macOS marker written before #329; it still counts as
	// proof the install is ours, and is what the repair path removes.
	LegacyBundleMarker bool
	// StateRecordMatches: the state-dir record names this very app bundle.
	StateRecordMatches bool
}

// managedFrom judges the facts per OS.
//
// darwin accepts either signal: the state-dir record (what fresh installs
// write now) or the legacy in-bundle marker (what pre-#329 installs left
// behind, on hosts the repair has not reached yet). Accepting the legacy one
// keeps a broken host recognisably ours, which is exactly what lets the
// repair path fix it instead of treating it as the user's own Ollama.
//
// windows/linux keep the same-dir marker as the only signal: their installs
// are plain directories with no seal to break, and the Windows repair arm
// (engineIncomplete) reads the ABSENCE of the marker as "extracted but never
// configured".
func managedFrom(goos string, f ManagedFacts) bool {
	if goos == "darwin" {
		return f.StateRecordMatches || f.LegacyBundleMarker
	}
	return f.MarkerBesideBinary
}

// gatherManagedFacts observes the three facts for the install at binPath.
// legacyMarker is "" unless a seal-breaking in-bundle marker actually exists,
// so callers can both judge and repair from one detection pass.
func gatherManagedFacts(stateDir, binPath string) (f ManagedFacts, legacyMarker string) {
	if fileExists(filepath.Join(filepath.Dir(binPath), WairedManagedMarkerName)) {
		f.MarkerBesideBinary = true
	}
	root := bundleRoot(binPath)
	if root != "" {
		if p := filepath.Join(root, WairedManagedMarkerName); fileExists(p) {
			f.LegacyBundleMarker = true
			legacyMarker = p
		}
		if rec := readDarwinManagedRecord(stateDir); rec != "" && rec == root {
			f.StateRecordMatches = true
		}
	}
	return f, legacyMarker
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RepairDarwinBundleMarker removes the seal-breaking in-bundle marker and
// re-records managed-ness in the state dir, so the bundle passes codesign
// again without waired forgetting that it owns the install.
//
// Removing the file is the whole repair: a bundle whose only unsealed content
// was that marker goes straight back to "valid on disk" / "accepted,
// Notarized Developer ID" once it is gone (proven on a live broken host, #329).
// No re-download is needed.
//
// It is idempotent and cheap enough to call on every setup pass. The bool
// reports whether anything was actually changed. A non-nil error with
// changed=true means the bundle was repaired but the replacement record could
// not be written — the install works, waired just may not recognise it as its
// own later.
func RepairDarwinBundleMarker(goos, stateDir string, det OllamaDetection) (changed bool, err error) {
	if goos != "darwin" || det.LegacyBundleMarkerPath == "" {
		return false, nil
	}
	if rmErr := os.Remove(det.LegacyBundleMarkerPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		// /Applications/Ollama.app is root-owned and not group-writable, so
		// an unprivileged CLI cannot do this. Say so rather than leaving the
		// operator with a bare EACCES.
		return false, fmt.Errorf(
			"remove %s (this needs administrator access — re-run with sudo): %w",
			det.LegacyBundleMarkerPath, rmErr)
	}
	if root := bundleRoot(det.Path); root != "" && stateDir != "" {
		if wErr := WriteDarwinManagedRecord(stateDir, root, DarwinManagedInstallerRepair); wErr != nil {
			return true, fmt.Errorf("record the waired-managed engine under %s: %w", stateDir, wErr)
		}
	}
	return true, nil
}
