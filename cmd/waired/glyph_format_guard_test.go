package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// markerGlyphs are the status marks this CLI degrades through emo() when the
// sink cannot render them. Kept in sync with asciiFolder's "status marks"
// block in ascii.go -- that table is the fallback for glyphs that reach the
// writer anyway, this one is the list a format string must not carry.
var markerGlyphs = []string{
	"⚠️", "⚠", "⬇️", "⬇", "✅", "✓", "✔", "✗", "✕", "●", "◐", "○",
	"ℹ️", "ℹ", "🎉", "🔌", "⏳", "⚡",
}

// glyphFormatAllowList names the fmt call sites whose format string may carry
// a marker glyph, with the reason. An allow-list rather than a match on the
// expected text: the defect this guards is a glyph nobody noticed, so the test
// has to fail on anything new rather than on a wording change
// (waired-agent#798 (d)).
var glyphFormatAllowList = map[string]string{
	// The systemMessage is JSON handed to Claude Code, which renders it in its
	// own UTF-8 UI. It never reaches a Windows console or a redirected log, so
	// folding it would degrade a surface that renders the glyph correctly.
	"claude_statusline.go:504": "JSON systemMessage consumed by Claude Code, not a console",
}

// TestFmtFormatStringsCarryNoBareMarkerGlyph is the regression guard for
// waired-agent#798 (d): `waired status > file` came out pure ASCII except for
// one U+26A0, because that one warning line hardcoded the glyph in its Printf
// format string instead of taking it from emo(). Its siblings on the same
// screen all degraded correctly, which is exactly why it survived review.
//
// Scope is deliberately narrow -- the FORMAT argument of an fmt call, not
// every string literal in the package. A glyph that arrives as a *value*
// (models_catalog's cell renderers, init_benchmark's emo() results) is already
// gated by the helper that produced it; a glyph baked into the format string
// bypasses every gate there is.
func TestFmtFormatStringsCarryNoBareMarkerGlyph(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "ascii.go" || name == "emoji.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "fmt" {
				return true
			}
			idx, ok := formatArgIndex(sel.Sel.Name)
			if !ok || len(call.Args) <= idx {
				return true
			}
			lit, ok := call.Args[idx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			seen++
			for _, g := range markerGlyphs {
				if !strings.Contains(text, g) {
					continue
				}
				pos := fset.Position(lit.Pos())
				key := name + ":" + strconv.Itoa(pos.Line)
				if _, allowed := glyphFormatAllowList[key]; allowed {
					return true
				}
				t.Errorf("%s: fmt.%s format string carries %q -- take it from emo(%q, \"<ascii>\") "+
					"so a redirected or non-UTF-8 sink gets the fallback, or add %s to "+
					"glyphFormatAllowList with a reason",
					key, sel.Sel.Name, g, g, key)
				return true
			}
			return true
		})
	}

	// Reachability: if the walk stops finding fmt calls at all (a refactor
	// moved output behind a helper, a parser change), this test would pass by
	// examining nothing. Assert it actually looked at something.
	if seen < 50 {
		t.Fatalf("only %d fmt format-string literals inspected; the guard is not reaching the package's output", seen)
	}
}

// formatArgIndex gives the position of the format string for the fmt
// functions that take one, and reports false for the rest.
func formatArgIndex(fn string) (int, bool) {
	switch fn {
	case "Printf", "Sprintf", "Errorf":
		return 0, true
	case "Fprintf":
		return 1, true
	case "Println", "Print", "Sprintln", "Sprint":
		// No format verb, but the first literal is still text this CLI writes.
		return 0, true
	case "Fprintln", "Fprint":
		return 1, true
	}
	return 0, false
}
