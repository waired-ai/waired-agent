package main

import (
	"sort"
	"strings"
	"testing"
)

var fixtureDirs = []string{"testdata/tree/cmd/waired"}

const fixtureFile = "testdata/tree/cmd/waired/reads.go"

func run(t *testing.T, want []client) []string {
	t.Helper()
	got, _, err := guard(fixtureDirs, want)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	sort.Strings(got)
	return got
}

func mustContain(t *testing.T, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, want) {
			return
		}
	}
	t.Errorf("no violation mentioning %q; got:\n  %s", want, strings.Join(got, "\n  "))
}

// Everything declared → clean. The fixture pins what "everything" means:
// both construction syntaxes, nothing from a _test.go file, and nothing
// from a same-named field on an unrelated type.
func TestGuard_FullyDeclaredIsClean(t *testing.T) {
	got := run(t, []client{
		{fixtureFile, "http.Client{}", "literal"},
		{fixtureFile, "http.DefaultClient", "package default"},
	})
	if len(got) != 0 {
		t.Errorf("want clean, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// The composite-literal syntax is caught. Three of the six #785 sites
// used this form, and a scan for http.DefaultClient alone would have
// passed a tree still containing them.
func TestGuard_UndeclaredCompositeLiteralFails(t *testing.T) {
	got := run(t, []client{
		{fixtureFile, "http.DefaultClient", "package default"},
	})
	mustContain(t, got, "http.Client{}")
}

// The package-default syntax is caught too — the other three sites.
func TestGuard_UndeclaredDefaultClientFails(t *testing.T) {
	got := run(t, []client{
		{fixtureFile, "http.Client{}", "literal"},
	})
	mustContain(t, got, "http.DefaultClient")
}

// A declaration outliving its call site fails, so the table stays a
// description of the code rather than a wish.
func TestGuard_StaleDeclarationFails(t *testing.T) {
	got := run(t, []client{
		{fixtureFile, "http.Client{}", "literal"},
		{fixtureFile, "http.DefaultClient", "package default"},
		{"testdata/tree/cmd/waired/gone.go", "http.Client{}", "deleted last week"},
	})
	mustContain(t, got, "no such HTTP client exists any more")
}

// A declaration with no reason is an error, not a pass: the reason is the
// whole point of the table.
func TestGuard_ReasonlessDeclarationIsAnError(t *testing.T) {
	if _, _, err := guard(fixtureDirs, []client{{fixtureFile, "http.Client{}", "  "}}); err == nil {
		t.Fatal("guard accepted a declaration with no reason")
	}
}

func TestGuard_DuplicateDeclarationIsAnError(t *testing.T) {
	dup := []client{
		{fixtureFile, "http.Client{}", "literal"},
		{fixtureFile, "http.Client{}", "literal again"},
	}
	if _, _, err := guard(fixtureDirs, dup); err == nil {
		t.Fatal("guard accepted a duplicate declaration")
	}
}

// The real table must describe the real tree. CI runs the binary from the
// repo root, but `go test` runs here, so the fixture-free check needs the
// declared paths rebased onto the same prefix the walk produces. Without
// this the guard could pass its own unit tests while disagreeing with the
// code it guards.
func TestDeclaredMatchesTheTree(t *testing.T) {
	const root = "../../../"
	rebased := make([]client, len(declared))
	for i, c := range declared {
		rebased[i] = client{File: root + c.File, Expr: c.Expr, Reason: c.Reason}
	}
	got, n, err := guard([]string{root + "cmd/waired"}, rebased)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cmd/waired disagrees with exemptions.go:\n  %s", strings.Join(got, "\n  "))
	}
	if n != len(declared) {
		t.Errorf("declared count = %d, want %d", n, len(declared))
	}
}
