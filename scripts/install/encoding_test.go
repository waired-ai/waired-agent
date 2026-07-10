package installscripts

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// mirroredPS1 lists the PowerShell installer scripts that ship to end users
// and are run via `iwr -useb …/install.ps1 | iex`. That one-liner coerces
// the downloaded bytes through the client's system ANSI code page, so any
// byte >= 0x80 (a UTF-8 emoji / box-drawing glyph, or a stray BOM) gets
// turned into "?" — the banner mojibake a user hit on a Japanese Windows
// host. Keeping these scripts pure-ASCII (non-ASCII glyphs built at runtime
// via Glyph / Utf8FromB64 in install.ps1) is the fix; this test guards
// against regressions. Paths are repo-relative, slash-separated.
var mirroredPS1 = []string{
	"packaging/install/install.ps1",
	"packaging/install/uninstall.ps1",
	"scripts/install/ollama-windows.ps1",
}

// repoRoot resolves the module root from this test file's compile-time path
// (<root>/scripts/install/encoding_test.go), independent of the test's CWD.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestInstallerPS1ScriptsArePureASCII(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range mirroredPS1 {
		t.Run(rel, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
				t.Errorf("%s starts with a UTF-8 BOM; `iwr|iex` turns it into a stray '?' — keep the file BOM-less", rel)
			}
			line, col := 1, 1
			for i, by := range b {
				if by == '\n' {
					line, col = line+1, 1
					continue
				}
				if by >= 0x80 {
					t.Fatalf("%s: non-ASCII byte 0x%02X at offset %d (line %d, col %d); installer scripts "+
						"must be pure-ASCII so `iwr|iex` does not mojibake them — build non-ASCII glyphs at "+
						"runtime via Glyph/Utf8FromB64 instead of embedding them literally", rel, by, i, line, col)
				}
				col++
			}
		})
	}
}
