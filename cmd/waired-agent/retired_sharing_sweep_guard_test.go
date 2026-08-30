package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The pre-1297 sharing files are deleted at boot, and that deletion must
// not depend on the inference subsystem.
//
// The reason they are deleted rather than read is written down in
// internal/runtime/state: a file nobody reads is one a later reader can
// resurrect with the wrong meaning, and these two answered a question the
// hard kill no longer asks. A daemon started with --disable-inference
// builds no sharing controller — and used to skip the sweep with it,
// leaving both files on disk for good (waired#1305).
//
// A source guard because there is no seam to call: the sweep runs once,
// inside the daemon's start-up, before anything a test could hold. What
// went wrong was placement, so placement is what this reads.
func TestRetiredSharingSweepIsNotGatedOnInference(t *testing.T) {
	const src = "main.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var stack []ast.Node
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RemoveRetiredSharingFiles" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "state" {
			return true
		}
		found++
		for _, anc := range stack {
			ifs, ok := anc.(*ast.IfStmt)
			if !ok {
				continue
			}
			ast.Inspect(ifs.Cond, func(c ast.Node) bool {
				if id, ok := c.(*ast.Ident); ok && id.Name == "disableInference" {
					t.Errorf("state.RemoveRetiredSharingFiles at %s is inside an `if` on disableInference; "+
						"a daemon started with --disable-inference would keep the pre-1297 files for good",
						fset.Position(call.Pos()))
				}
				return true
			})
		}
		return true
	})

	if found != 1 {
		t.Fatalf("found %d calls to state.RemoveRetiredSharingFiles in %s, want exactly 1", found, src)
	}
}
