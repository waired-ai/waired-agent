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
