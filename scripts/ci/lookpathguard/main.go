// Command lookpathguard makes every exec.LookPath call site in cmd/ and
// internal/ a declared one.
//
// Why this exists: "is X installed" answered from $PATH alone has been
// wrong here five separate times — #67's nvidia-smi detection, the
// deploy path, #179's wizard engine state, the setup-install probe, and
// #238's engine-version probe, which this guard's own table surfaced.
// The reason is structural, and cmd/waired-agent/engine_resolve.go
// states it: the engine waired installs for itself lives under the state
// dir and is deliberately NOT on $PATH, and a LocalSystem service on
// Windows does not inherit a user PATH at all. A PATH-only probe
// therefore reports "not installed" on exactly the hosts waired set up
// itself, while the daemon — which resolves the binary by stat —
// disagrees at the same instant on the same machine. That disagreement
// is the head of the G1 chain in waired#932: pull never admitted, step
// contents frozen, wizard "offline", lease expired, executor_gone.
//
// #179 collapsed five mutually contradicting engine predicates into one
// (resolveOllamaBinary), and #238 removed the last straggler this table
// had frozen. This guard is the other half: it stops them growing back. Probing a system tool — sudo, systemctl, zenity — is
// fine and stays fine; what must not happen silently is a NEW call site
// deciding whether a waired-managed component is present.
//
// Every existing call site is declared in exemptions.go with the binary
// it probes and why PATH is the right question there. The table is
// checked in both directions: an undeclared call site fails, and so does
// a declaration whose call site is gone.
//
// Usage: lookpathguard [dir...]   (default: cmd internal)
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	dirs := []string{"cmd", "internal"}
	if len(os.Args) > 1 {
		dirs = os.Args[1:]
	}
	violations, n, err := guard(dirs, declared)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lookpathguard: %v\n", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Printf("::error::%s\n", v)
		}
		fmt.Fprint(os.Stderr, help)
		os.Exit(1)
	}
	fmt.Printf("lookpathguard: OK (%d declared exec.LookPath call sites)\n", n)
}

const help = `
exec.LookPath answers "is this name on $PATH", which is NOT the same
question as "is this component installed" — and the difference has
produced the same defect four times (#67, the deploy path, #179, the
setup-install probe).

waired's own engine lives under the state dir and is deliberately off
$PATH; a Windows LocalSystem service inherits no user PATH at all. So a
PATH-only probe says "missing" on precisely the hosts waired set up
itself, while the daemon, which stats the binary, says "present" at the
same instant. Resolve waired-managed binaries through the single
predicate in cmd/waired-agent/engine_resolve.go instead.

If the binary you are probing is a SYSTEM tool the host either has or
does not (sudo, runuser, systemctl, zenity, nvidia-smi, uv), PATH is the
right question. Add the call site to declared in
scripts/ci/lookpathguard/exemptions.go with the reason.

Entries are checked both ways: an undeclared call site fails, and a
declaration whose call site no longer exists fails too — so the table
stays a description of the code rather than a wish.
`

type site struct{ File, Binary string }

func (s site) String() string { return s.File + " → " + s.Binary }

// guard returns one violation per undeclared call site and one per
// declaration with no matching call site.
func guard(dirs []string, want []lookpath) ([]string, int, error) {
	found := map[site]int{}
	for _, dir := range dirs {
		if err := collectLookPaths(dir, found); err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", dir, err)
		}
	}
	allowed := map[site]bool{}
	for _, w := range want {
		s := site{filepath.ToSlash(w.File), w.Binary}
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
			out = append(out, fmt.Sprintf("%s: undeclared exec.LookPath call site", s))
		}
	}
	for _, s := range sortedSites(allowed) {
		if _, ok := found[s]; !ok {
			out = append(out, fmt.Sprintf(
				"%s: declared, but no such exec.LookPath call site exists any more — delete the entry", s))
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

// collectLookPaths records every exec.LookPath call in non-test Go files
// under root, keyed by (file, argument). A non-literal argument is
// recorded under its source expression, which is stable enough to
// declare and loud enough to notice.
func collectLookPaths(root string, into map[site]int) error {
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
			call, ok := n.(*ast.CallExpr)
			if !ok || !isLookPath(call.Fun) || len(call.Args) != 1 {
				return true
			}
			into[site{rel, argName(call.Args[0])}]++
			return true
		})
		return nil
	})
}

func isLookPath(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "LookPath" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "exec"
}

// argName is the probed binary for a string literal, and the raw
// expression otherwise — `ollamaCmdName` reads better in the table than
// "<dynamic>", and it is the identifier a reviewer would grep for.
func argName(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s
		}
	}
	return types.ExprString(e)
}
