package main

import (
	"strings"
	"testing"
)

// The host stopped being a typed field and became a derived one
// (waired-agent#1455). These rows pin the order of precedence, which is
// the whole of the change: an observation the measuring host made of
// itself beats a claim someone typed, and a flag that contradicts it is
// refused rather than obeyed.
//
// This is decision 20260829/1100 §1's shape, applied to the last
// hand-typed field in the provenance set — the same treatment
// --engine-version got there.
func TestSettleTurnSpeedHost(t *testing.T) {
	derived := func(hosts ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, h := range hosts {
			m[h] = struct{}{}
		}
		return m
	}
	for _, tc := range []struct {
		name          string
		flag, stored  string
		derived       map[string]struct{}
		want          string
		wantErrSubstr string
	}{
		{
			name:    "the snapshots say it and nobody typed anything",
			derived: derived("unified-amd-ryzen-ai-max-395"),
			want:    "unified-amd-ryzen-ai-max-395",
		},
		{
			name:    "the flag agrees with the snapshots",
			flag:    "unified-amd-ryzen-ai-max-395",
			derived: derived("unified-amd-ryzen-ai-max-395"),
			want:    "unified-amd-ryzen-ai-max-395",
		},
		{
			name:          "the flag contradicts the snapshots",
			flag:          "discrete-nvidia-sm120",
			derived:       derived("unified-amd-ryzen-ai-max-395"),
			wantErrSubstr: "contradicts the snapshots",
		},
		{
			name:          "snapshots from two different machines",
			derived:       derived("unified-amd-ryzen-ai-max-395", "discrete-nvidia-sm120"),
			wantErrSubstr: "more than one kind of machine",
		},
		{
			name:    "an agent from before the key existed: the flag is all there is",
			flag:    "discrete-nvidia-sm120",
			derived: derived(),
			want:    "discrete-nvidia-sm120",
		},
		{
			name:          "no key and no flag",
			derived:       derived(),
			wantErrSubstr: "nobody can place",
		},
		{
			name:          "a derived key that would mix classes in an existing store",
			stored:        "unified-amd-ryzen-ai-max-395",
			derived:       derived("discrete-nvidia-sm120"),
			wantErrSubstr: "would mix host classes",
		},
		{
			name:    "a legacy spelling continuing the store it already names",
			flag:    "amd-unified-128gb",
			stored:  "amd-unified-128gb",
			derived: derived(),
			want:    "amd-unified-128gb",
		},
		{
			name:          "a legacy spelling starting a store",
			flag:          "amd-unified-128gb",
			derived:       derived(),
			wantErrSubstr: "frozen legacy spelling",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := settleTurnSpeedHost(tc.flag, tc.stored, tc.derived)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("settleTurnSpeedHost() = %q, nil; want an error mentioning %q",
						got, tc.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("error = %v, want it to mention %q", err, tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("settleTurnSpeedHost() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("settleTurnSpeedHost() = %q, want %q", got, tc.want)
			}
		})
	}
}
