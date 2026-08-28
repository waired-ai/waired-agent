package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guards in this package excuse a site by a comment written at that site
// rather than by naming it in a list: `// glyph: <why>` for a marker glyph in
// an fmt format string, `// ascii: <why>` for output that must not be folded.
//
// A list keyed by position is what waired-agent#1103 removed. All three
// commits that touched the glyph guard after it was introduced changed only a
// line number, and two lanes growing the same file collided on that line on
// every rebase. A marker moves with the code it is attached to, and the reason
// is in front of the person reading that line. CLAUDE.md §Test discipline
// carries the rule; scripts/ci/tray-grey-row-guard.sh is the same shape for
// `// grey: <why>`.
//
// This file is the one implementation, so a second guard cannot drift from the
// first on what counts as a marker or on which direction it is checked.

// sourceMarker is one `// <prefix> <why>` comment: where it is, what it says,
// and whether the walk ever found a site for it to excuse.
type sourceMarker struct {
	file   string
	line   int // the line the marker comment itself begins on
	last   int // the last line of its comment group -- the reason may run on
	reason string
	used   bool
}

// collectMarkers reads every `<prefix> <why>` comment in one file. The reason
// may run onto the following lines of the same comment group, so the group's
// last line is what a site has to follow to be excused.
func collectMarkers(fset *token.FileSet, file string, f *ast.File, prefix string) []*sourceMarker {
	var out []*sourceMarker
	for _, group := range f.Comments {
		var m *sourceMarker
		var reason []string
		for _, c := range group.List {
			text := strings.TrimSpace(c.Text)
			if m == nil {
				if !strings.HasPrefix(text, prefix) {
					continue
				}
				m = &sourceMarker{file: file, line: fset.Position(c.Pos()).Line}
				text = strings.TrimPrefix(text, prefix)
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

// markerFor finds the marker covering a site spanning lines from..to: either
// the comment group directly above it, or a comment written inside its own
// line range. Both forms read naturally -- a long call takes the marker above,
// a short one takes it trailing.
func markerFor(markers []*sourceMarker, from, to int) *sourceMarker {
	for _, m := range markers {
		if m.last == from-1 || (m.line >= from && m.line <= to) {
			return m
		}
	}
	return nil
}

// reportMarkerRot is the other direction of every marker-keyed guard: a marker
// with no reason says nothing, and a marker that excused nothing is a stale
// claim. The allow-list this convention replaced had neither check, so an
// entry whose site had moved would have sat there unread.
//
// excuses says what the marker is for, in the shape "no fmt call on or just
// below it prints to stdout" -- it is the second half of the sentence the
// developer reads when the marker is dead.
func reportMarkerRot(t *testing.T, markers []*sourceMarker, prefix, excuses string) {
	t.Helper()
	for _, m := range markers {
		switch {
		case m.reason == "":
			t.Errorf("%s:%d: this `%s` marker gives no reason. It is worth four words: "+
				"say why this site is the exception.", m.file, m.line, prefix)
		case !m.used:
			t.Errorf("%s:%d: this `%s` marker excuses nothing -- %s. Remove it.",
				m.file, m.line, prefix, excuses)
		}
	}
}

// parsePackageSources parses the package's own non-test files, with comments,
// skipping the two that define the fold and the emoji gate themselves: their
// tables list the very characters a guard is looking for.
//
// Returns the files in glob order so a failure list is stable, and the names as
// the glob produced them (bare, no directory) so a failure reads the way the
// developer's editor addresses the file.
func parsePackageSources(t *testing.T, fset *token.FileSet) ([]string, map[string]*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := make(map[string]*ast.File, len(names))
	var kept []string
	for _, name := range names {
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
		kept = append(kept, name)
		out[name] = f
	}
	if len(kept) == 0 {
		t.Fatal("no package sources parsed; the guards are not looking at anything")
	}
	return kept, out
}
