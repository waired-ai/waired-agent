package main

import (
	"sort"
	"strings"
	"testing"
)

var fixtureDirs = []string{"testdata/tree/cmd", "testdata/tree/internal"}

func run(t *testing.T, want []lookpath) []string {
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

// Everything declared → clean, and the fixture pins what "everything"
// means: a literal argument by binary name, a non-literal one by its
// source expression, nothing from _test.go, nothing from a same-named
// method on another type.
func TestGuard_FullyDeclaredIsClean(t *testing.T) {
	got := run(t, []lookpath{
		{"testdata/tree/cmd/a/probe.go", "sudo", "privilege helper"},
		{"testdata/tree/cmd/a/probe.go", "binary", "engine version probe"},
		{"testdata/tree/internal/b/probe.go", "zenity", "desktop helper"},
	})
	if len(got) != 0 {
		t.Errorf("want clean, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// The case the guard exists for: a new PATH probe appears and nobody
// wrote down why PATH is the right question.
func TestGuard_UndeclaredCallSiteFails(t *testing.T) {
	got := run(t, []lookpath{
		{"testdata/tree/cmd/a/probe.go", "sudo", "privilege helper"},
		{"testdata/tree/cmd/a/probe.go", "binary", "engine version probe"},
	})
	mustContain(t, got, "testdata/tree/internal/b/probe.go → zenity: undeclared exec.LookPath call site")
	if len(got) != 1 {
		t.Errorf("want exactly 1 violation, got %d:\n  %s", len(got), strings.Join(got, "\n  "))
	}
}

// The other direction. Without this the table slowly becomes a list of
// probes that used to exist, and stops describing the code.
func TestGuard_DeclarationWithoutCallSiteFails(t *testing.T) {
	got := run(t, []lookpath{
		{"testdata/tree/cmd/a/probe.go", "sudo", "privilege helper"},
		{"testdata/tree/cmd/a/probe.go", "binary", "engine version probe"},
		{"testdata/tree/internal/b/probe.go", "zenity", "desktop helper"},
		{"testdata/tree/internal/b/probe.go", "kdialog", "removed last release"},
	})
	mustContain(t, got, "kdialog: declared, but no such exec.LookPath call site exists any more")
}

// A probe in a _test.go file decides nothing on a user's machine, and
// requiring a declaration for it would train people to declare by rote.
func TestGuard_IgnoresTestFiles(t *testing.T) {
	got := run(t, []lookpath{
		{"testdata/tree/cmd/a/probe.go", "sudo", "privilege helper"},
		{"testdata/tree/cmd/a/probe.go", "binary", "engine version probe"},
		{"testdata/tree/internal/b/probe.go", "zenity", "desktop helper"},
	})
	for _, g := range got {
		if strings.Contains(g, "kubectl") {
			t.Errorf("collected a probe from a _test.go file: %s", g)
		}
	}
}

func TestGuard_RejectsMalformedTable(t *testing.T) {
	cases := []struct {
		name string
		in   []lookpath
		want string
	}{
		{"no reason", []lookpath{{"a.go", "sudo", "  "}}, "has no reason"},
		{"duplicate", []lookpath{
			{"a.go", "sudo", "why"},
			{"a.go", "sudo", "why again"},
		}, "duplicate entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := guard(fixtureDirs, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The declared table describes this repository as it is right now, so a
// moved or deleted probe fails `go test ./...` as well as the lint step.
func TestRepoTableIsCurrent(t *testing.T) {
	const root = "../../.."
	got, n, err := guard([]string{root + "/cmd", root + "/internal"}, withRoot(root, declared))
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("guard is not clean against the repo:\n  %s", strings.Join(got, "\n  "))
	}
	if n == 0 {
		t.Fatal("no declarations resolved — the fixture paths are probably wrong")
	}
}

// withRoot re-bases the declared repo-relative paths onto the path the
// test walks from, so the table itself stays readable as repo paths.
func withRoot(root string, in []lookpath) []lookpath {
	out := make([]lookpath, len(in))
	for i, e := range in {
		e.File = root + "/" + e.File
		out[i] = e
	}
	return out
}
