package runtime

import (
	"fmt"
	"os"
	"runtime"
)

// assertExecutable reports nil iff path exists, is a regular file, and —
// where the bit means anything — carries an execute permission.
//
// Windows has no execute permission bit: Go reports 0666 (or 0444 for a
// read-only file) for every file there, so demanding one would reject
// every install on the OS. Executability on Windows is decided by the
// extension, which BundledOllamaBinaryPath already supplies.
//
// Untagged (it used to live in the linux-only uv.go) because the bundled
// Ollama installer is cross-platform since #492.
func assertExecutable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable: %s", path)
	}
	return nil
}
