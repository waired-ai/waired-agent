package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// LookPath wraps exec.LookPath so adapters can swap it in tests via
// a local package var if needed. Today they call os/exec directly.
func LookPath(binary string) (string, bool) {
	p, err := exec.LookPath(binary)
	if err != nil {
		return "", false
	}
	return p, true
}

// DirExists is true when path resolves to a directory. Symlinks are
// followed (Stat, not Lstat) since most tools' config dirs are real
// dirs but some users symlink ~/.claude into a dotfiles repo.
func DirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ConfigDirHasForeignEntry reports whether dir contains at least one
// top-level entry whose name is not in owned. It lets an adapter's
// Detect tell a real agent install (config dir populated with the
// agent's own files) apart from a bare config dir that waired's own
// Apply pre-provisioned — a plain DirExists check self-poisons because
// every Apply MkdirAll's a child of the very dir Detect keys on
// (waired#753). owned lists the basenames of the children waired itself
// creates under dir. A missing or unreadable dir yields false (Detect
// must not surface an error). Non-recursive: only the top level is read.
func ConfigDirHasForeignEntry(dir string, owned ...string) bool {
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	ownedSet := make(map[string]struct{}, len(owned))
	for _, name := range owned {
		ownedSet[name] = struct{}{}
	}
	for _, e := range entries {
		if _, ok := ownedSet[e.Name()]; !ok {
			return true
		}
	}
	return false
}

// HomeJoin joins home with rel using filepath.Join. Returns "" when
// home is empty (caller should treat that as "no signal").
func HomeJoin(home, rel string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, rel)
}

// ExecutableFileExists is true when path resolves to a regular file
// with at least one execute bit set. Symlinks are followed (Stat).
// On Windows the exec-bit check is skipped — file permissions don't
// model executability there.
func ExecutableFileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}
