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
//
// There is no exemption marker, deliberately: a rune with no way to become
// ASCII has only two honest fixes, a fold entry or an emo() call, and both are
// one line. `// ascii:` excuses a print site from the fold, which is a
// different claim, so a marker here would read as covering something it does
// not.
func TestNonASCIILiteralsCanDegrade(t *testing.T) {
	fset := token.NewFileSet()
	names, files := parsePackageSources(t, fset)

	seen := 0
	for _, name := range names {
		f := files[name]
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
// json.NewEncoder(os.Stdout) is one of those exceptions rather than a hole in
// the walk: JSON must never be folded, and the marker is where that is said.
//
// The rule covers two shapes, because a print seam can be walked past in two
// ways. `fmt.Println(x)` names no writer and goes to the process's stdout; and
// `printSetupHelper(target, opts, os.Stdout, os.Stdin)` hands the raw file to a
// renderer that writes it with fmt.Fprintln. The second is how link_helper.go's
// em dashes reached a CP932 console: the print site looked innocent because the
// writer was a parameter. Both live in one test so that one collection of
// `// ascii:` markers has one owner -- a marker consumed by one walk would look
// stale to the other.
//
// Reading os.Stdout rather than writing to it is not this guard's business and
// is exempt by shape: isTerminal(os.Stdout) needs the *os.File, style_windows
// needs its handle, and pii_mask.go swaps the file itself.
func TestPrintedOutputGoesThroughTheFold(t *testing.T) {
	fset := token.NewFileSet()
	names, files := parsePackageSources(t, fset)

	var markers []*sourceMarker
	seen := 0
	for _, name := range names {
		f := files[name]
		fileMarkers := collectMarkers(fset, name, f, asciiMarkerPrefix)
		markers = append(markers, fileMarkers...)

		excuse := func(node ast.Node) bool {
			from := fset.Position(node.Pos()).Line
			to := fset.Position(node.End()).Line
			m := markerFor(fileMarkers, from, to)
			if m == nil {
				return false
			}
			m.used = true
			return true
		}

		readOnly := standardStreamReads(f)

		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" {
					return true
				}
				if !printsToStandardStream(sel.Sel.Name, v.Args) {
					return true
				}
				seen++
				if excuse(v) {
					return true
				}
				t.Errorf("%s:%d: fmt.%s writes to the process's standard stream, which no fold\n"+
					"    can reach -- a Windows console on a code page that is not CP_UTF8, a\n"+
					"    redirected stream on Windows, and a terminal on a non-UTF-8 locale all\n"+
					"    decode those bytes as something else (waired-agent#629). Write to this\n"+
					"    package's stdout / stderr instead. If this output must NOT be folded --\n"+
					"    a model's own text, a JSON body, the Claude Code status line -- say so\n"+
					"    above the call:\n"+
					"        %s the model's generated text, served verbatim",
					name, fset.Position(v.Pos()).Line, sel.Sel.Name, asciiMarkerPrefix)
				return true

			case *ast.SelectorExpr:
				pkg, ok := v.X.(*ast.Ident)
				if !ok || pkg.Name != "os" {
					return true
				}
				if v.Sel.Name != "Stdout" && v.Sel.Name != "Stderr" {
					return true
				}
				if readOnly[v.Pos()] {
					return true
				}
				seen++
				if excuse(v) {
					return true
				}
				t.Errorf("%s:%d: os.%s is handed out here, and a renderer that writes to it with\n"+
					"    fmt.Fprint* bypasses the fold exactly as a bare fmt.Print would. Pass\n"+
					"    this package's %s instead. If the callee must NOT fold -- it encodes\n"+
					"    JSON, it is a child process's stream, it is the Claude Code status line\n"+
					"    -- say so above it:\n"+
					"        %s a JSON encoder; folding would edit the payload",
					name, fset.Position(v.Pos()).Line, v.Sel.Name,
					foldedName(v.Sel.Name), asciiMarkerPrefix)
				return true
			}
			return true
		})
	}

	reportMarkerRot(t, markers, asciiMarkerPrefix,
		"nothing on or just below it writes to a standard stream")

	// Reachability: this guard reports nothing once the package is clean, so
	// without a floor a refactor that hid every print behind a helper would
	// look identical to a green run. `seen` counts the sites it inspected,
	// exempt ones included -- there were 26 on 2026-08-29.
	if seen < 10 {
		t.Fatalf("only %d unfolded output sites inspected; the guard is not reaching the package's output", seen)
	}
}

// foldedName maps Stdout / Stderr to the folding writer that replaces it.
func foldedName(stream string) string {
	if stream == "Stderr" {
		return "stderr"
	}
	return "stdout"
}

// printsToStandardStream reports whether an fmt call writes to the process's
// own stdout or stderr, rather than to a writer the caller chose. Sprintf and
// Errorf do not print at all.
func printsToStandardStream(fn string, args []ast.Expr) bool {
	switch fn {
	case "Print", "Printf", "Println":
		return true
	case "Fprint", "Fprintf", "Fprintln":
		if len(args) == 0 {
			return false
		}
		sel, ok := args[0].(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "os" && (sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr")
	}
	return false
}

// standardStreamReads returns the positions of os.Stdout / os.Stderr mentions
// that ask something about the file rather than write to it: isTerminal's
// argument, a field or method on it (Fd()), and the left side of the swap
// pii_mask.go performs. None of those can carry a byte to a console.
func standardStreamReads(f *ast.File) map[token.Pos]bool {
	out := map[token.Pos]bool{}
	mark := func(e ast.Expr) {
		if sel, ok := e.(*ast.SelectorExpr); ok {
			out[sel.Pos()] = true
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if fn, ok := v.Fun.(*ast.Ident); ok && fn.Name == "isTerminal" {
				for _, a := range v.Args {
					mark(a)
				}
			}
		case *ast.SelectorExpr:
			// os.Stdout.Fd() and friends: the inner selector is the read.
			mark(v.X)
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				mark(lhs)
			}
		}
		return true
	})
	return out
}
