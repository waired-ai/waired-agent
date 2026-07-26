package main

import (
	"testing"
	"time"
)

func TestOllamaInstallTimeout(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset falls back to the backstop", "", ollamaInstallTimeoutDefault},
		{"raises the backstop for a slow link", "3h", 3 * time.Hour},
		{"lowers it", "90s", 90 * time.Second},
		// A typo in an escape hatch must not be a harder failure than not
		// setting it: the install still runs, just on the default bound.
		{"garbage falls back", "soon", ollamaInstallTimeoutDefault},
		{"zero falls back", "0", ollamaInstallTimeoutDefault},
		{"negative falls back", "-5m", ollamaInstallTimeoutDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaInstallTimeout(func(k string) string {
				if k != ollamaInstallTimeoutEnv {
					t.Fatalf("looked up %q, want %q", k, ollamaInstallTimeoutEnv)
				}
				return tc.env
			})
			if got != tc.want {
				t.Errorf("ollamaInstallTimeout(%q) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// The old fixed bound killed a healthy ~1.43 GB download at 15 minutes; the
// backstop has to leave room for the slow links that were failing.
func TestOllamaInstallTimeoutDefaultClearsASlowDownload(t *testing.T) {
	const (
		archiveBytes     = 1_451_523_701 // the size measured on the affected host
		slowLinkBytesSec = 1.5 * 1024 * 1024
	)
	seconds := float64(archiveBytes) / slowLinkBytesSec
	need := time.Duration(seconds * float64(time.Second))
	if ollamaInstallTimeoutDefault <= need {
		t.Errorf("default backstop %v does not cover a %v download at 1.5 MB/s",
			ollamaInstallTimeoutDefault, need)
	}
}
