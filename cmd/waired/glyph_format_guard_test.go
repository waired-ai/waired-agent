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
// read that site will see it.
//
// It replaces a `file:line` allow-list that lived in this test. The key was a
// coordinate into a file other people edit, so every change above the exempt
// site invalidated it: all three commits that touched this guard after it was
// introduced changed only that number (334 -> 384 -> 514 -> 583) while the
// reason stayed byte-identical, and two lanes growing claude_statusline.go
// collided on that one line on every rebase (waired-agent#1103). A marker moves
// with the code it is attached to. The shape is the one
// scripts/ci/tray-grey-row-guard.sh already uses for `// grey: <why>`.
const glyphMarkerPrefix = "// glyph:"

// glyphMarker is one `// glyph: <why>` comment: where it is, what it says, and
// whether the walk ever found a site for it to exempt.
type glyphMarker struct {
	file   string
	line   int // the line the `// glyph:` comment itself begins on
	last   int // the last line of its comment group -- the reason may run on
	reason string
	used   bool
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
//
// Scope is also cmd/waired only, because that is where the sink can be a
// Windows console or a redirected log. The one other package with glyphs in a
// format string is internal/proxy/intercept's reroute notice, which is markdown
// handed to Claude Code and rendered in its own UTF-8 UI -- folding it there
// would degrade a surface that shows the glyph correctly. Widening the walk to
// that package would buy five exemptions and catch nothing.
func TestFmtFormatStringsCarryNoBareMarkerGlyph(t *testing.T) {
	glyphs := markerGlyphs()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	var markers []*glyphMarker
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "ascii.go" || name == "emoji.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		fileMarkers := collectGlyphMarkers(fset, name, f)
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
				if m := glyphMarkerFor(fileMarkers, from, to); m != nil {
					m.used = true
					return true
				}
				t.Errorf("%s:%d: fmt.%s format string carries %q -- take it from emo(%q, \"<ascii>\")\n"+
					"    so a redirected or non-UTF-8 sink gets the fallback. If this one really does\n"+
					"    reach a surface that renders it correctly, say so above the call:\n"+
					"        %s the systemMessage is JSON, rendered by Claude Code's own UTF-8 UI",
					name, part.line, sel.Sel.Name, g, g, glyphMarkerPrefix)
				return true
			}
			return true
		})
	}

	// The other direction: a marker that exempts nothing is a stale claim. The
	// allow-list this replaced had no such check, so an entry whose site had
	// moved or lost its glyph would have sat there unread.
	for _, m := range markers {
		switch {
		case m.reason == "":
			t.Errorf("%s:%d: this `%s` marker gives no reason. It is worth four words: "+
				"say what surface renders the glyph correctly.", m.file, m.line, glyphMarkerPrefix)
		case !m.used:
			t.Errorf("%s:%d: this `%s` marker exempts nothing -- no fmt format string on or "+
				"just below it carries a marker glyph. Remove it.", m.file, m.line, glyphMarkerPrefix)
		}
	}

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

// collectGlyphMarkers reads every `// glyph: <why>` comment in one file. The
// reason may run onto the following lines of the same comment group, so the
// group's last line is what a call has to follow to be exempt.
func collectGlyphMarkers(fset *token.FileSet, file string, f *ast.File) []*glyphMarker {
	var out []*glyphMarker
	for _, group := range f.Comments {
		var m *glyphMarker
		var reason []string
		for _, c := range group.List {
			text := strings.TrimSpace(c.Text)
			if m == nil {
				if !strings.HasPrefix(text, glyphMarkerPrefix) {
					continue
				}
				m = &glyphMarker{file: file, line: fset.Position(c.Pos()).Line}
				text = strings.TrimPrefix(text, glyphMarkerPrefix)
			} else {
				text = strings.TrimPrefix(text, "//")
			}
			if s := strings.TrimSpace(text); s != "" {
				reason = append(reason, s)
			}
		}
		if m == nil {
			continue
		}
		m.last = fset.Position(group.End()).Line
		m.reason = strings.Join(reason, " ")
		out = append(out, m)
	}
	return out
}

// glyphMarkerFor finds the marker covering a call spanning lines from..to:
// either the comment group directly above it, or a comment written inside its
// own line range. Both forms read naturally -- a long format string takes the
// marker above, a short one takes it trailing.
func glyphMarkerFor(markers []*glyphMarker, from, to int) *glyphMarker {
	for _, m := range markers {
		if m.last == from-1 || (m.line >= from && m.line <= to) {
			return m
		}
	}
	return nil
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
