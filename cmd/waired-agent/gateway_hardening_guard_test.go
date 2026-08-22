package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestEveryGatewayListenerSetsBrowserHardening pins the wiring, not the
// guard: internal/gateway owns the Host/Origin checks and its own tests
// prove they work when ServerConfig.BrowserHardening is set. What no test in
// that package can see is whether this package actually sets it.
//
// That gap is not hypothetical. Before waired-ai/waired#1277 the Local
// Gateway constructed a ServerConfig with only Addr, so the allow-list was
// off on the one loopback listener a browser was documented to talk to, and
// every test in internal/gateway was green anyway.
//
// A source guard rather than a behavioural test because the composition
// happens inside gateway.NewServer at a call site buried in
// startInferenceSubsystem, which needs a live engine, a registry and a
// selector to reach. The property worth defending is one line long and
// reads directly off the AST.
func TestEveryGatewayListenerSetsBrowserHardening(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	found := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewServer" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "gateway" {
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				lit, ok := call.Args[0].(*ast.CompositeLit)
				if !ok {
					return true
				}
				found++
				where := fset.Position(call.Pos()).String()
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "BrowserHardening" {
						return true
					}
				}
				t.Errorf("gateway.NewServer at %s builds a ServerConfig without BrowserHardening — "+
					"a loopback listener with no allow-list and no credential is reachable by any page "+
					"the user has open (waired-ai/waired#1195, #1277)", where)
				return true
			})
		}
	}

	// Without this the guard passes vacuously the day someone renames the
	// constructor or moves the call out of this package.
	if found == 0 {
		t.Fatal("no gateway.NewServer call site found in cmd/waired-agent — the guard is not looking at anything")
	}
}

// TestOnlyOneLoopbackGatewayListener pins the collapse itself: this package
// binds exactly one gateway listener, not two.
//
// There used to be a second on 9479 serving the same routes from the same
// handler set, which existed because the desktop user could not read the
// bearer token the first one required. Both are gone
// (waired-ai/waired#1277). A second net.Listen would mean the split had
// grown back — and the thing that makes that easy to do by accident is that
// the two call sites looked almost identical, differing only in which policy
// fields they set.
func TestOnlyOneLoopbackGatewayListener(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var sites []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewServer" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "gateway" {
					return true
				}
				sites = append(sites, fset.Position(call.Pos()).String())
				return true
			})
		}
	}

	if len(sites) != 1 {
		t.Fatalf("gateway.NewServer call sites = %d %v, want exactly 1 — "+
			"local inference is served by one listener (waired-ai/waired#1277)", len(sites), sites)
	}
}
