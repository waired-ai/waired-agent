package pwsh

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestChildEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{
			// The spelling PowerShell 7 actually exports.
			name: "drops PSModulePath as PowerShell spells it",
			env:  []string{"PATH=/bin", `PSModulePath=C:\Program Files\PowerShell\7\Modules`, "HOME=/root"},
			want: []string{"PATH=/bin", "HOME=/root"},
		},
		{
			// Windows environment names are case-insensitive, so no single
			// spelling can be assumed.
			name: "drops every casing",
			env:  []string{"PSMODULEPATH=a", "psmodulepath=b", "PsModulePath=c", "KEEP=1"},
			want: []string{"KEEP=1"},
		},
		{
			name: "no-op when absent (the Unix case)",
			env:  []string{"PATH=/usr/bin", "SHELL=/bin/sh"},
			want: []string{"PATH=/usr/bin", "SHELL=/bin/sh"},
		},
		{
			// A variable that merely starts with the same prefix must stay.
			name: "keeps look-alike names",
			env:  []string{"PSMODULEPATH_BACKUP=x", "MYPSMODULEPATH=y"},
			want: []string{"PSMODULEPATH_BACKUP=x", "MYPSMODULEPATH=y"},
		},
		{
			// Windows exposes per-drive working directories as "=C:=C:\dir".
			// strings.Cut splits those with an empty key; they must survive.
			name: "keeps Windows per-drive entries",
			env:  []string{`=C:=C:\Windows`, "PSModulePath=x"},
			want: []string{`=C:=C:\Windows`},
		},
		{
			name: "empty",
			env:  nil,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.env)
			got := ChildEnv(tc.env)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ChildEnv(%q) = %q, want %q", tc.env, got, tc.want)
			}
			if !slices.Equal(tc.env, in) {
				t.Errorf("ChildEnv mutated its input: %q, was %q", tc.env, in)
			}
		})
	}
}

func TestEnvStripsPSModulePath(t *testing.T) {
	t.Setenv("PSModulePath", `C:\Program Files\PowerShell\7\Modules`)
	t.Setenv("WAIRED_PWSH_TEST_SENTINEL", "1")

	var sawSentinel bool
	for _, kv := range Env() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PSMODULEPATH") {
			t.Errorf("Env() still carries %q", kv)
		}
		if k == "WAIRED_PWSH_TEST_SENTINEL" {
			sawSentinel = true
		}
	}
	if !sawSentinel {
		t.Error("Env() dropped an unrelated variable")
	}
	// The process environment itself must be untouched: only the child's
	// copy is sanitized.
	if os.Getenv("PSModulePath") == "" {
		t.Error("Env() cleared PSModulePath in the parent process")
	}
}
