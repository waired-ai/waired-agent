package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"testing"
)

// asciiMarkerPrefix introduces the exemption for output that must NOT be
// folded. Four kinds of site qualify and every one of them would be corrupted
// by folding, so the marker says which: a model's own generated text, a JSON
// body served verbatim, and the Claude Code status line.
const asciiMarkerPrefix = "// ascii:"

// TestNonASCIILiteralsCanDegrade is the guard for the half of waired-agent#629
// that PR #674 could not reach by hand: a non-ASCII character this CLI writes
// has to have a way to become ASCII when the sink cannot carry the bytes.
//
// Two ways exist, and one of them must apply to every non-ASCII rune in a
// string literal here:
//
//   - asciiFolder rewrites it at the sink (ascii.go). This is the one that
//     works for prose -- the em dashes and ellipses that made up the reported
//     mojibake -- because prose reaches the writer as itself.
//   - emo() / slGlyph() pick the fallback before the character is ever built
//     into the string. This is the one that works for marks, where the ASCII
//     stand-in is a different shape rather than a transliteration.
//
// A rune with neither is a character that reaches a CP932 console as raw UTF-8
// with nothing able to stop it. Three did (waired-agent#1105): `◦` U+25E6,
// `↓` U+2193 and `⋯` U+22EF, all in `waired models ls --detail`'s state column
// and legend. Eight more were latent, saved only by an emo() call that a later
// edit could have dropped.
//
// Deliberately NOT asserted: that a literal is ASCII. Runes with no fold entry
// pass through on purpose (ascii.go's own doc, and TestAsciiFold's negative
// half) because a device name, a user name and a model id can all be
// non-ASCII, and mangling those would be worse than the bug this fixes. The
// question here is only whether the character CAN degrade, not whether it does.
func TestNonASCIILiteralsCanDegrade(t *testing.T) {
	fset := token.NewFileSet()
	names, files := parsePackageSources(t, fset)

	var markers []*sourceMarker
	seen := 0
	for _, name := range names {
		f := files[name]
		fileMarkers := collectMarkers(fset, name, f, asciiMarkerPrefix)
		markers = append(markers, fileMarkers...)

		gated := glyphGateArguments(f)

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			seen++
			bad, line := firstNonASCII(asciiFold(text))
			if bad == 0 || gated[lit.Pos()] {
				return true
			}
			pos := fset.Position(lit.Pos())
			t.Errorf("%s:%d: %q (U+%04X) survives asciiFold and is not an emo() fallback, in:\n"+
				"    %s\n"+
				"    Give it an entry in statusMarkFolds (ascii.go) so the fold can degrade it,\n"+
				"    or take it from emo(%q, \"<ascii>\") so it is never built into the string.",
				name, pos.Line, string(bad), bad, line, string(bad))
			return true
		})
	}

	reportMarkerRot(t, markers, asciiMarkerPrefix,
		"nothing on or just below it prints to stdout or stderr")

	// Reachability, in the shape TestFmtFormatStringsCarryNoBareMarkerGlyph
	// uses: 6,000-odd string literals were in scope on 2026-08-29, so a floor
	// of 2,000 catches the walk going blind without moving on ordinary churn
	// (a record of today's reach, not a contract).
	if seen < 2000 {
		t.Fatalf("only %d string literals inspected; the guard is not reaching the package", seen)
	}
}

// glyphGateArguments returns the positions of the literals that emo() and
// slGlyph() take as their glyph argument. Those never reach a sink that cannot
// draw them: the helper has already chosen the ASCII stand-in by then.
//
// Keyed by position rather than by text so that the same glyph hardcoded
// somewhere else in the file is still reported.
func glyphGateArguments(f *ast.File) map[token.Pos]bool {
	out := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || (fn.Name != "emo" && fn.Name != "slGlyph") {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out[lit.Pos()] = true
		}
		return true
	})
	return out
}

// TestPrintedOutputGoesThroughTheFold is the other half of waired-agent#1105.
//
// asciiFold can only rewrite what passes through plainText, and plainText has
// exactly two callers: writePrompt and writePromptf (init_prompt.go). Every
// bare fmt.Print* writes to the real os.Stdout, which no fold can reach --
// which is why #629's mojibake survived on every surface except the `waired
// init` transcript. `waired inference status`, the first line of #629's own
// symptom report, was still mangled two months after that issue was closed.
//
// So the rule is the sink, not the site: output goes to this package's stdout
// and stderr (ascii.go), which fold when the sink cannot carry the bytes.
// cobra writes its help through the same pair via root.SetOut / root.SetErr.
//
// The exceptions all share a shape -- folding them would corrupt data rather
// than degrade prose -- and each says so with `// ascii: <why>`:
//
//   - a model's generated text, served verbatim;
//   - a JSON body from the management API, likewise;
//   - the Claude Code status line, whose stdout is a pipe into Claude Code's
//     own UTF-8 UI. That pipe is precisely where foldOutput() reports true on
//     Windows, so folding it would degrade the one surface that renders it
//     correctly.
//
// json.NewEncoder(os.Stdout) is outside the walk on purpose: JSON must never
// be folded, so there is nothing for a marker to decide.
func TestPrintedOutputGoesThroughTheFold(t *testing.T) {
	fset := token.NewFileSet()
	names, files := parsePackageSources(t, fset)

	var markers []*sourceMarker
	seen := 0
	for _, name := range names {
		f := files[name]
		fileMarkers := collectMarkers(fset, name, f, asciiMarkerPrefix)
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
			sink, ok := unfoldedSink(sel.Sel.Name, call.Args)
			if !ok {
				return true
			}
			seen++

			from := fset.Position(call.Pos()).Line
			to := fset.Position(call.End()).Line
			if m := markerFor(fileMarkers, from, to); m != nil {
				m.used = true
				return true
			}
			t.Errorf("%s:%d: fmt.%s writes to %s, which no fold can reach -- a Windows console\n"+
				"    on a code page that is not CP_UTF8, a redirected stream on Windows, and a\n"+
				"    terminal on a non-UTF-8 locale all decode those bytes as something else\n"+
				"    (waired-agent#629). Write to this package's %s instead:\n"+
				"        fmt.F%s(%s, ...)\n"+
				"    If this output must NOT be folded -- a model's own text, a JSON body, the\n"+
				"    Claude Code status line -- say so above the call:\n"+
				"        %s the model's generated text, served verbatim",
				name, from, sel.Sel.Name, sink, foldedName(sink), sel.Sel.Name, foldedName(sink),
				asciiMarkerPrefix)
			return true
		})
	}

	reportMarkerRot(t, markers, asciiMarkerPrefix,
		"nothing on or just below it prints to stdout or stderr")

	// Reachability: this guard reports nothing once the package is clean, so
	// without a floor a refactor that hid every print behind a helper would
	// look identical to a green run. `seen` counts the sites it inspected,
	// exempt ones included -- there were 13 on 2026-08-29.
	if seen < 5 {
		t.Fatalf("only %d unfolded print sites inspected; the guard is not reaching the package's output", seen)
	}
}

// unfoldedSink names the standard stream an fmt call writes to without passing
// through the fold, and reports false for calls that are not printing at all
// (Sprintf, Errorf) or that write to a writer the caller chose.
func unfoldedSink(fn string, args []ast.Expr) (string, bool) {
	switch fn {
	case "Print", "Printf", "Println":
		return "os.Stdout", true
	case "Fprint", "Fprintf", "Fprintln":
		if len(args) == 0 {
			return "", false
		}
		sel, ok := args[0].(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return "", false
		}
		if sel.Sel.Name != "Stdout" && sel.Sel.Name != "Stderr" {
			return "", false
		}
		return "os." + sel.Sel.Name, true
	}
	return "", false
}

// foldedName maps os.Stdout / os.Stderr to the folding writer that replaces it.
func foldedName(sink string) string {
	if sink == "os.Stderr" {
		return "stderr"
	}
	return "stdout"
}
