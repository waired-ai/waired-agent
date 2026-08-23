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

// gatewayNewServerSites returns every `gateway.NewServer(...)` call in this
// package's non-test files, as "file:line" plus the ServerConfig literal it
// was handed (nil when the first argument is not a composite literal).
//
// Reading the AST rather than exercising the code because the composition
// happens inside startInferenceSubsystem, which needs a live engine, a
// registry and a selector to reach. Both properties below are one line long
// and read directly off the syntax.
func gatewayNewServerSites(t *testing.T) (sites []string, configs []*ast.CompositeLit) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
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
			var cfg *ast.CompositeLit
			if len(call.Args) > 0 {
				cfg, _ = call.Args[0].(*ast.CompositeLit)
			}
			configs = append(configs, cfg)
			return true
		})
	}
	return sites, configs
}

// TestEveryGatewayListenerSetsBrowserHardening pins the wiring, not the
// guard: internal/gateway owns the Host/Origin checks and its own tests
// prove they work when ServerConfig.BrowserHardening is set. What no test in
// that package can see is whether this package actually sets it.
//
// That gap is not hypothetical. Before waired-ai/waired#1277 the Local
// Gateway constructed a ServerConfig with only Addr, so the allow-list was
// off on the one loopback listener a browser was documented to talk to, and
// every test in internal/gateway was green anyway.
func TestEveryGatewayListenerSetsBrowserHardening(t *testing.T) {
	sites, configs := gatewayNewServerSites(t)

	// Without this the guard passes vacuously the day someone renames the
	// constructor or moves the call out of this package.
	if len(sites) == 0 {
		t.Fatal("no gateway.NewServer call site found in cmd/waired-agent — the guard is not looking at anything")
	}

	for i, cfg := range configs {
		if cfg == nil {
			t.Errorf("gateway.NewServer at %s is not handed a ServerConfig literal — the guard cannot see whether it is hardened", sites[i])
			continue
		}
		hardened := false
		for _, elt := range cfg.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "BrowserHardening" {
				hardened = true
				break
			}
		}
		if !hardened {
			t.Errorf("gateway.NewServer at %s builds a ServerConfig without BrowserHardening — "+
				"a loopback listener with no allow-list and no credential is reachable by any page "+
				"the user has open (waired-ai/waired#1195, #1277)", sites[i])
		}
	}
}

// TestOnlyOneLoopbackGatewayListener pins the collapse itself: this package
// binds exactly one gateway listener, not two.
//
// There used to be a second on 9479 serving the same routes from the same
// handler set, which existed because the desktop user could not read the
// bearer token the first one required. Both are gone
// (waired-ai/waired#1277). A second net.Listen would mean the split had
// grown back — and what makes that easy to do by accident is that the two
// call sites looked almost identical, differing only in which policy fields
// they set.
func TestOnlyOneLoopbackGatewayListener(t *testing.T) {
	sites, _ := gatewayNewServerSites(t)
	if len(sites) != 1 {
		t.Fatalf("gateway.NewServer call sites = %d %v, want exactly 1 — "+
			"local inference is served by one listener (waired-ai/waired#1277)", len(sites), sites)
	}
}
