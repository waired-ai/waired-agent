package installscripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The installers use the CLI's status glyphs (⚠ ✅ 🎉 ⬆ ℹ) with the CLI's ASCII
// fallbacks, so a run that hands off to `waired init` reads as one program
// (owner decision 2026-09-08, docs/decisions/20260908/*-installer-copy.md).
// This holds every `emo` / `Emo` site in the four scripts against the fold
// table in cmd/waired/ascii.go: a glyph the table does not know may not be
// used, and a fallback that differs from the table's is a drift.
func TestInstallerEmoFallbacksMatchTheFoldTable(t *testing.T) {
	root := repoRoot(t)
	table := foldTable(t, filepath.Join(root, "cmd", "waired", "ascii.go"))

	shSite := regexp.MustCompile(`emo '([^']*)' '([^']*)'`)
	psSite := regexp.MustCompile(`Emo \(Glyph (0x[0-9A-Fa-f]+)\) '([^']*)'`)

	for _, rel := range []string{
		"packaging/install/install.sh",
		"packaging/install/uninstall.sh",
	} {
		src := readFile(t, filepath.Join(root, rel))
		for _, m := range shSite.FindAllStringSubmatch(src, -1) {
			checkGlyph(t, table, rel, m[1], m[2])
		}
	}
	for _, rel := range []string{
		"packaging/install/install.ps1",
		"packaging/install/uninstall.ps1",
	} {
		src := readFile(t, filepath.Join(root, rel))
		for _, m := range psSite.FindAllStringSubmatch(src, -1) {
			cp, err := strconv.ParseInt(strings.TrimPrefix(m[1], "0x"), 16, 32)
			if err != nil {
				t.Fatalf("%s: bad code point %q: %v", rel, m[1], err)
			}
			checkGlyph(t, table, rel, string(rune(cp)), m[2])
		}
	}
}

func checkGlyph(t *testing.T, table map[string]string, rel, glyph, fallback string) {
	t.Helper()
	want, ok := table[glyph]
	if !ok {
		t.Errorf("%s: glyph %q is not in cmd/waired/ascii.go's statusMarkFolds; use one the CLI uses, or none", rel, glyph)
		return
	}
	if fallback != want {
		t.Errorf("%s: glyph %q falls back to %q, the CLI's table says %q", rel, glyph, fallback, want)
	}
}

// foldTable reads the statusMarkFolds pairs out of ascii.go's source: the
// slice is a flat list of "glyph", "fallback" string literals.
func foldTable(t *testing.T, path string) map[string]string {
	t.Helper()
	src := readFile(t, path)
	start := strings.Index(src, "var statusMarkFolds = []string{")
	if start < 0 {
		t.Fatalf("%s: statusMarkFolds not found", path)
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatalf("%s: statusMarkFolds has no closing brace", path)
	}
	body := src[start : start+end]
	lit := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	var items []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		for _, m := range lit.FindAllStringSubmatch(line, -1) {
			s, err := strconv.Unquote(`"` + m[1] + `"`)
			if err != nil {
				t.Fatalf("%s: cannot unquote %q: %v", path, m[1], err)
			}
			items = append(items, s)
		}
	}
	if len(items) == 0 || len(items)%2 != 0 {
		t.Fatalf("%s: statusMarkFolds parsed into %d literals, want an even, non-zero count", path, len(items))
	}
	table := map[string]string{}
	for i := 0; i+1 < len(items); i += 2 {
		table[items[i]] = items[i+1]
	}
	return table
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
