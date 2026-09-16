//go:build linux || darwin

package claudemanaged

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteLeavesTheDirectoryItCreatesReadable runs a write under the 027
// umask a hardened host gives its administrators. sudo keeps the caller's
// umask when it is stricter than its own 022, so `sudo waired claude enable`
// there used to create /etc/claude-code as 0750 root:root: the file inside
// was 0644, and no one but root could reach it. Claude Code reads it as the
// user who runs `claude`.
//
// A directory that already exists keeps its mode — an operator may have
// restricted it on purpose — and Windows is untouched: there the directory
// inherits Program Files' ACL, and mode bits mean nothing.
//
// Serial and top-level on purpose: the umask belongs to the whole process.
//
// PIN: product contract, waired-agent#1419.
func TestWriteLeavesTheDirectoryItCreatesReadable(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)

	write := func(t *testing.T, goos, dir string) {
		t.Helper()
		restore := SwapPathForTest(filepath.Join(dir, "managed-settings.json"))
		defer restore()
		if _, err := writeWithOptionsFor(goos, "http://127.0.0.1:9472", WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	modeOf := func(t *testing.T, path string) fs.FileMode {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}

	for _, tc := range []struct {
		goos string
		want fs.FileMode
	}{
		{"linux", 0o755},
		{"darwin", 0o755},
		{"windows", 0o750}, // not chmod'ed; 0755 less this umask
	} {
		t.Run(tc.goos+" creates the directory", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "claude-code")
			write(t, tc.goos, dir)
			if got := modeOf(t, dir); got != tc.want {
				t.Errorf("directory mode = %#o, want %#o", got, tc.want)
			}
			if got := modeOf(t, filepath.Join(dir, "managed-settings.json")); got != 0o644 {
				t.Errorf("file mode = %#o, want 0644", got)
			}
		})
	}

	t.Run("an existing directory keeps its mode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "claude-code")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, "linux", dir)
		if got := modeOf(t, dir); got != 0o700 {
			t.Errorf("directory mode = %#o, want the operator's 0700", got)
		}
	})
}
