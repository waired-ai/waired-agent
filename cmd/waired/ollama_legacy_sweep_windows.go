//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Migration off the pre-#493 Windows layout.
//
// Until #493 the installer put Ollama in %ProgramFiles%\Ollama, prepended
// that directory to the machine PATH, and wrote OLLAMA_MODELS /
// OLLAMA_VULKAN / OLLAMA_IGPU_ENABLE at machine scope. The engine now lives
// under the state dir and the agent supplies the environment at spawn, so
// all of it is dead weight — and the GPU variables are worse than dead: the
// daemon only drops an inherited OLLAMA_* value when its own backend plan
// sets the same key, so a stale machine OLLAMA_VULKAN=1 leaks into a spawn
// on a host whose plan chose CUDA.
//
// The consequence we accept is a one-time re-download (~1.5 GB) on hosts
// that had the old layout: the two installs share nothing.

// wairedManagedMarkerName is the receipt the retired ollama-windows.ps1 wrote
// beside ollama.exe as the LAST step of a successful install. It was the only
// way to tell waired's install from the user's own at a path they shared, and
// it is kept here — the last reader in the tree — solely so this sweep can
// prove an install was ours before deleting it.
const wairedManagedMarkerName = ".waired-managed.json"

// machineEnvKey is where Windows keeps machine-scope environment variables.
const machineEnvKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// legacyGPUEnvNames are the machine-scope variables ollama-windows.ps1 wrote.
var legacyGPUEnvNames = []string{"OLLAMA_MODELS", "OLLAMA_VULKAN", "OLLAMA_IGPU_ENABLE"}

// sweepLegacyOllamaInstall removes a %ProgramFiles%\Ollama that waired
// itself installed, along with the machine PATH entry and GPU variables
// that went with it.
//
// Only waired's own install is touched, and only on the evidence waired
// itself wrote: the marker file beside ollama.exe. A user's own Ollama —
// unmarked, or the per-user %LOCALAPPDATA%\Programs\Ollama layout the
// official installer uses — is never swept. Getting this wrong deletes
// software the operator installed deliberately, so the check is the
// narrow one.
//
// Best-effort throughout: it runs AFTER a successful state-dir install, so
// the engine already works and a failure here costs disk, not function.
func sweepLegacyOllamaInstall(getenv func(string) string, out io.Writer) {
	pf := getenv("ProgramFiles")
	if pf == "" {
		return
	}
	dir := filepath.Join(pf, "Ollama")
	if !legacyInstallIsOurs(dir) {
		return
	}
	_, _ = fmt.Fprintf(out, "Removing the previous Ollama install at %s (Waired's own copy; the engine now lives with Waired's data)...\n", dir)
	if err := os.RemoveAll(dir); err != nil {
		_, _ = fmt.Fprintf(out, "Warning: couldn't remove %s: %v\n", dir, err)
	}
	if err := removeFromMachinePath(dir); err != nil {
		_, _ = fmt.Fprintf(out, "Warning: could not take %s off the machine PATH: %v\n", dir, err) // vocab: Windows names the HKLM environment scope "Machine"
	}
	for _, name := range legacyGPUEnvNames {
		if err := deleteMachineEnv(name); err != nil {
			_, _ = fmt.Fprintf(out, "Warning: could not clear the machine %s variable: %v\n", name, err) // vocab: Windows names the HKLM environment scope "Machine"
		}
	}
}

// legacyInstallIsOurs reports whether dir holds an Ollama waired installed:
// an ollama.exe with waired's marker file beside it.
func legacyInstallIsOurs(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "ollama.exe")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, wairedManagedMarkerName))
	return err == nil
}

// removeFromMachinePath drops dir from the machine PATH, leaving every
// other entry and their order alone. A no-op when it is not there.
func removeFromMachinePath(dir string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	cur, valType, err := k.GetStringValue("Path")
	if err != nil {
		return err
	}
	next, changed := pathWithout(cur, dir)
	if !changed {
		return nil
	}
	// Preserve REG_EXPAND_SZ: the machine PATH normally contains %SystemRoot%
	// references, and rewriting it as a plain string would freeze them.
	if valType == registry.EXPAND_SZ {
		return k.SetExpandStringValue("Path", next)
	}
	return k.SetStringValue("Path", next)
}

// pathWithout removes every entry of path that names dir, comparing the way
// Windows does: case-insensitively, either separator, trailing slash
// ignored. Pure, so the comparison is table-testable without a registry.
func pathWithout(path, dir string) (string, bool) {
	want := normalizeWindowsPath(dir)
	kept := make([]string, 0, 16)
	changed := false
	for _, entry := range strings.Split(path, ";") {
		if entry == "" {
			continue
		}
		if normalizeWindowsPath(entry) == want {
			changed = true
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, ";"), changed
}

// normalizeWindowsPath lower-cases, unifies separators and drops a trailing
// one, so two spellings of the same Windows path compare equal. Windows
// paths are case-insensitive and accept either separator.
func normalizeWindowsPath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, `/`, `\`))
	return strings.TrimRight(p, `\`)
}

// deleteMachineEnv removes a machine-scope environment variable. Absent is
// success: the variable being gone is the whole objective.
func deleteMachineEnv(name string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineEnvKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	if err := k.DeleteValue(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
