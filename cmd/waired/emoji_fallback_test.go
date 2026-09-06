package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"testing"
)

// TestEmoFallbacksMatchTheFoldTable pins that every emo(symbol, fallback) call
// site names the fallback the fold table would produce for the same symbol.
//
// PIN: product contract — docs/decisions/20260907/*-product-copy-conventions.md
// (waired-agent#1277). Two paths degrade a glyph to ASCII: emo() on a terminal
// that cannot render emoji, and asciiFold on a Windows console whose code page
// cannot carry the bytes. Before this pin the two disagreed at the call site —
// ✅ was "*" in one box and "[ok]" in the next, ⚠ was "!" or "[!]" — so the
// same event read differently depending on which gate caught it. The table in
// ascii.go is the single source; a site that wants a different fallback
// changes the table (and every sibling with it), not its own argument.
func TestEmoFallbacksMatchTheFoldTable(t *testing.T) {
	fset := token.NewFileSet()
	names, files := parsePackageSources(t, fset)
	checked := 0
	for _, name := range names {
		ast.Inspect(files[name], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "emo" || len(call.Args) != 2 {
				return true
			}
			sym, ok1 := call.Args[0].(*ast.BasicLit)
			fb, ok2 := call.Args[1].(*ast.BasicLit)
			if !ok1 || !ok2 {
				return true
			}
			symbol, err1 := strconv.Unquote(sym.Value)
			fallback, err2 := strconv.Unquote(fb.Value)
			if err1 != nil || err2 != nil {
				return true
			}
			checked++
			if want := asciiFold(symbol); fallback != want {
				pos := fset.Position(call.Pos())
				t.Errorf("%s:%d: emo(%q, %q) — the fold table degrades %q to %q; use that, or change the table for every site",
					pos.Filename, pos.Line, symbol, fallback, symbol, want)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no emo(symbol, fallback) call sites — the pin is not looking at what it thinks")
	}
}
