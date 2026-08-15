// Command mgmtclientguard makes every HTTP client built inside
// cmd/waired a declared one.
//
// Why this exists: since waired#836 the daemon serves only five
// allow-listed GET routes over the loopback TCP port while its local IPC
// socket is bound (internal/management/socket.go, tcpReadRoutes), and
// refuses every other read with 403. The CLI has one helper that knows
// this — mgmtReadRoute in cmd/waired/main.go, which sends over the socket
// with a TCP fallback — and six call sites had quietly built their own
// client instead. All six failed in production, on three routes:
//
//	/waired/v1/inference/mesh          `waired peers list` and
//	                                   `waired worker set --pin=<name>`
//	                                   exited 1; `waired doctor` silently
//	                                   dropped its measured mesh line
//	/waired/v1/identity                `waired init` lost the daemon
//	                                   authority #313 added for it, and
//	                                   the doctor's network-connection row
//	                                   vanished
//	/waired/v1/integration/claude/route the Claude Code statusline rendered
//	                                   "agent down" on every turn of a
//	                                   healthy machine, and the post-turn
//	                                   fallback notice never fired
//
// None of that was visible as a build or test failure, because the CLI
// and the daemon each behaved correctly in isolation (#785).
//
// A scan for http.DefaultClient would not have caught it: three of the
// six sites used that, and three built &http.Client{...}. So this guard
// looks for BOTH, and every legitimate one — the gateway, release
// downloads, the control plane, and the allow-listed reads — is declared
// in exemptions.go with the reason it does not belong on the management
// helper. The table is checked in both directions: an undeclared client
// fails, and so does a declaration whose site is gone.
//
// Usage: mgmtclientguard [dir...]   (default: cmd/waired)
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	dirs := []string{filepath.Join("cmd", "waired")}
	if len(os.Args) > 1 {
		dirs = os.Args[1:]
	}
	violations, n, err := guard(dirs, declared)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mgmtclientguard: %v\n", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Printf("::error::%s\n", v)
		}
		fmt.Fprint(os.Stderr, help)
		os.Exit(1)
	}
	fmt.Printf("mgmtclientguard: OK (%d declared HTTP clients in %s)\n", n, strings.Join(dirs, " "))
}

const help = `
An HTTP client built here decides its own transport. For a MANAGEMENT
read that decision is already made and is not yours: since waired#836 the
daemon answers 403 on the loopback TCP port for every route outside
tcpReadRoutes (internal/management/socket.go), so a read must go over the
local IPC socket.

Route it through mgmtReadRoute (cmd/waired/main.go), or httpGet for the
common case. Writes go through mgmtWriteRoute, which exempts the /ping
liveness probe exactly as the daemon's writeGuard does.

If the client is NOT talking to the local management API — the coding-
agent gateway on :9479, a release download, the control plane, the
Anthropic API — add it to declared in
scripts/ci/mgmtclientguard/exemptions.go with the reason.

Entries are checked both ways: an undeclared client fails, and a
declaration whose site no longer exists fails too, so the table stays a
description of the code rather than a wish.
`

type site struct{ File, Expr string }

func (s site) String() string { return s.File + " → " + s.Expr }

// guard returns one violation per undeclared client and one per
// declaration with no matching site.
func guard(dirs []string, want []client) ([]string, int, error) {
	found := map[site]int{}
	for _, dir := range dirs {
		if err := collectClients(dir, found); err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", dir, err)
		}
	}
	allowed := map[site]bool{}
	for _, w := range want {
		s := site{filepath.ToSlash(w.File), w.Expr}
		if strings.TrimSpace(w.Reason) == "" {
			return nil, 0, fmt.Errorf("declared: %s has no reason", s)
		}
		if allowed[s] {
			return nil, 0, fmt.Errorf("declared: duplicate entry for %s", s)
		}
		allowed[s] = true
	}

	var out []string
	for _, s := range sortedSites(found) {
		if !allowed[s] {
			out = append(out, fmt.Sprintf("%s: undeclared HTTP client — management reads must use mgmtReadRoute", s))
		}
	}
	for _, s := range sortedSites(allowed) {
		if _, ok := found[s]; !ok {
			out = append(out, fmt.Sprintf(
				"%s: declared, but no such HTTP client exists any more — delete the entry", s))
		}
	}
	return out, len(allowed), nil
}

func sortedSites[V any](m map[site]V) []site {
	out := make([]site, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// collectClients records every HTTP client construction in non-test Go
// files under root. Two syntaxes count, because the defect used both:
// a composite literal of http.Client, and a reference to
// http.DefaultClient.
func collectClients(root string, into map[site]int) error {
	fset := token.NewFileSet()
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if isHTTPClientType(v.Type) {
					into[site{rel, "http.Client{}"}]++
				}
			case *ast.SelectorExpr:
				if isHTTPDefaultClient(v) {
					into[site{rel, "http.DefaultClient"}]++
				}
			}
			return true
		})
		return nil
	})
}

func isHTTPClientType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

func isHTTPDefaultClient(sel *ast.SelectorExpr) bool {
	if sel.Sel.Name != "DefaultClient" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}
