package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// markerGlyphs are the status marks this CLI degrades through emo() when the
// sink cannot render them.
//
// Derived from ascii.go's statusMarkFolds rather than copied. The two are the
// same set seen from two sides -- that one is the fallback for a glyph that
// reaches the writer anyway, this one is the list a format string must not
// carry -- and until waired-agent#1103 this file kept its own copy with a
// "kept in sync" comment and nothing checking it. A glyph added to one side and
// not the other would have left this guard silently blind to exactly the kind
// of character it exists to catch.
func markerGlyphs() []string {
	out := make([]string, 0, len(statusMarkFolds)/2)
	for i := 0; i < len(statusMarkFolds); i += 2 {
		out = append(out, statusMarkFolds[i])
	}
	return out
}

// glyphMarkerPrefix introduces the in-source exemption: a call site whose
// format string may carry a marker glyph says why, where the next person to
// read that site will see it. The machinery is shared with the fold guards --
// see source_markers_test.go for why an exemption is keyed at the site.
const glyphMarkerPrefix = "// glyph:"

// TestFmtFormatStringsCarryNoBareMarkerGlyph is the regression guard for
// waired-agent#798 (d): `waired status > file` came out pure ASCII except for
// one U+26A0, because that one warning line hardcoded the glyph in its Printf
// format string instead of taking it from emo(). Its siblings on the same
// screen all degraded correctly, which is exactly why it survived review.
//
// Scope is deliberately narrow -- the FORMAT argument of an fmt call, not
// every string literal in the package. A glyph that arrives as a *value*
// (models_catalog's cell renderers, init_benchmark's emo() results) is a
// different question, and TestNonASCIILiteralsCanDegrade is where it is asked:
// can this character become ASCII at all? Here the question is narrower --
// a glyph baked into a format string bypasses emo() specifically.
//
// Scope is also cmd/waired only, because that is where the sink can be a
// Windows console, a redirected stream, or a terminal on a non-UTF-8 locale.
// The one other package with glyphs in a format string is
// internal/proxy/intercept's reroute notice, which is markdown handed to
// Claude Code and rendered in its own UTF-8 UI -- folding it there would
// degrade a surface that shows the glyph correctly. Widening the walk to that
// package would buy five exemptions and catch nothing.
func TestFmtFormatStringsCarryNoBareMarkerGlyph(t *testing.T) {
	glyphs := markerGlyphs()

	fset := token.NewFileSet()
	names, files := parsePackageSources(t, fset)

	var markers []*sourceMarker
	seen := 0
	for _, name := range names {
		f := files[name]
		fileMarkers := collectMarkers(fset, name, f, glyphMarkerPrefix)
		markers = append(markers, fileMarkers...)

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
			parts := formatStringParts(fset, call.Args[idx])
			if len(parts) == 0 {
				return true
			}
			seen++

			from := fset.Position(call.Pos()).Line
			to := fset.Position(call.End()).Line
			for _, part := range parts {
				g, carries := firstMarkerGlyph(part.text, glyphs)
				if !carries {
					continue
				}
				if m := markerFor(fileMarkers, from, to); m != nil {
					m.used = true
					return true
				}
				t.Errorf("%s:%d: fmt.%s format string carries %q -- take it from emo(%q, \"<ascii>\")\n"+
					"    so a sink that cannot render it gets the fallback. If this one really does\n"+
					"    reach a surface that renders it correctly, say so above the call:\n"+
					"        %s rendered by Claude Code's own UTF-8 UI, never a console",
					name, part.line, sel.Sel.Name, g, g, glyphMarkerPrefix)
				return true
			}
			return true
		})
	}

	reportMarkerRot(t, markers, glyphMarkerPrefix,
		"no fmt format string on or just below it carries a marker glyph")

	// Reachability: if the walk stops finding fmt calls at all (a refactor
	// moved output behind a helper, a parser change), this test would pass by
	// examining nothing. Assert it actually looked at something. 665 format
	// strings were in scope on 2026-08-28; the floor is set well under that so
	// ordinary churn does not move it, but high enough to notice the walk going
	// blind to most of the package -- which the previous floor of 50 would not
	// have (a record of today's reach, not a contract).
	if seen < 400 {
		t.Fatalf("only %d fmt format-string literals inspected; the guard is not reaching the package's output", seen)
	}
}

// stringPart is one string literal of a format argument, with the line it is
// written on so a violation points at the right place in a multi-line
// concatenation.
type stringPart struct {
	text string
	line int
}

// formatStringParts returns every string literal in a format argument,
// flattening the `"a" + "b"` chains Go uses to write a long message across
// several lines.
//
// Until waired-agent#1103 the guard only read a lone *ast.BasicLit, so a
// concatenated format string was an *ast.BinaryExpr and passed through
// unexamined -- and a concatenation is what every long warning in this package
// is. Seven of them were invisible.
//
// An operand that is not a string literal is skipped rather than abandoning the
// whole expression, so the literal half of `"prefix " + v` is still read. Parts
// are returned separately and checked separately: joining them first could
// splice a glyph out of two harmless halves.
func formatStringParts(fset *token.FileSet, e ast.Expr) []stringPart {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return formatStringParts(fset, v.X)
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return nil
		}
		text, err := strconv.Unquote(v.Value)
		if err != nil {
			return nil
		}
		return []stringPart{{text: text, line: fset.Position(v.Pos()).Line}}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return nil
		}
		return append(formatStringParts(fset, v.X), formatStringParts(fset, v.Y)...)
	}
	return nil
}

// firstMarkerGlyph reports the first marker glyph text carries. The
// variation-selector forms come first in statusMarkFolds, so the more specific
// spelling is the one named in the failure.
func firstMarkerGlyph(text string, glyphs []string) (string, bool) {
	for _, g := range glyphs {
		if strings.Contains(text, g) {
			return g, true
		}
	}
	return "", false
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
