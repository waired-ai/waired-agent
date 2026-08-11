package main

import (
	"strings"
	"testing"
)

// TestOllamaDownloadHint_StatesTheRealSize pins that the size in the hint
// comes from the transfer, not from the sentence.
//
// The old text said "a few hundred MB" for every OS. That was written for
// macOS's ~129 MB payload and was wrong by an order of magnitude on Linux,
// where the CUDA payload makes it 1.4 GB — the operator read "a few hundred
// MB" and then watched 1.4 GB arrive (#661). Any fixed phrase is a claim
// about a payload that changes without this file.
//
// Product contract, ratified by #661.
func TestOllamaDownloadHint_StatesTheRealSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stage     string
		total     int64
		wantHas   []string
		wantNotIn []string
	}{
		{
			name:      "linux engine payload",
			stage:     "download",
			total:     1_400_000_000,
			wantHas:   []string{"Ollama engine", "1.4 GB"},
			wantNotIn: []string{"few hundred"},
		},
		{
			name:      "macos engine payload",
			stage:     "download",
			total:     129_000_000,
			wantHas:   []string{"Ollama engine", "129.0 MB"},
			wantNotIn: []string{"few hundred", "GB"},
		},
		{
			name:    "rocm runtime",
			stage:   "download-rocm",
			total:   320_000_000,
			wantHas: []string{"ROCm GPU runtime", "320.0 MB"},
		},
		{
			// No Content-Length: say nothing about the size rather than
			// guess one, which is how the old wording went wrong.
			name:      "length not advertised",
			stage:     "download",
			total:     0,
			wantHas:   []string{"Ollama engine", "a few minutes"},
			wantNotIn: []string{"MB", "GB", "few hundred"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaDownloadHint(tc.stage, tc.total)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("hint = %q, want it to contain %q", got, want)
				}
			}
			for _, bad := range tc.wantNotIn {
				if strings.Contains(got, bad) {
					t.Errorf("hint = %q, want it NOT to contain %q", got, bad)
				}
			}
		})
	}
}
