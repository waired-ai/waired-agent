package integration

import (
	"strings"
	"testing"
)

// TestInstallationFinding pins #652: a config directory with no binary on
// PATH used to render as
// `✓ openclaw installation — binary= configDir=/home/<user>/.openclaw`
// — a tick asserting an installation that is not there, with an empty
// field where the evidence should be. Observed on both Linux and macOS.
//
// Product contract from #652, not a record of today's behaviour. Shared
// across adapters so claude-code and openclaw cannot answer the same
// question differently.
func TestInstallationFinding(t *testing.T) {
	cases := []struct {
		name       string
		det        Detection
		wantStatus Status
		wantIn     []string
		wantNotIn  []string
	}{
		{
			name:       "binary on PATH is an installation",
			det:        Detection{Found: true, BinaryPath: "/usr/bin/openclaw", ConfigDir: "/home/u/.openclaw"},
			wantStatus: StatusOK,
			wantIn:     []string{"binary=/usr/bin/openclaw", "configDir=/home/u/.openclaw"},
		},
		{
			name:       "binary on PATH with no config dir yet is still an installation",
			det:        Detection{Found: true, BinaryPath: "/usr/bin/openclaw"},
			wantStatus: StatusOK,
			wantIn:     []string{"binary=/usr/bin/openclaw"},
		},
		{
			name:       "a config dir without a binary is not an installation",
			det:        Detection{Found: true, ConfigDir: "/home/u/.openclaw"},
			wantStatus: StatusSkip,
			wantIn:     []string{"not on PATH", "~/.openclaw is present"},
			wantNotIn:  []string{"binary="},
		},
		{
			name:       "neither is absent",
			det:        Detection{},
			wantStatus: StatusSkip,
			wantIn:     []string{"not on PATH", "absent"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := InstallationFinding("openclaw", "openclaw", "~/.openclaw", c.det)
			if got.Status != c.wantStatus {
				t.Errorf("status = %s, want %s (detail %q)", got.Status, c.wantStatus, got.Detail)
			}
			if got.Subject != "openclaw installation" {
				t.Errorf("subject = %q", got.Subject)
			}
			for _, w := range c.wantIn {
				if !strings.Contains(got.Detail, w) {
					t.Errorf("detail = %q, want it to contain %q", got.Detail, w)
				}
			}
			for _, w := range c.wantNotIn {
				if strings.Contains(got.Detail, w) {
					t.Errorf("detail = %q, must not contain %q", got.Detail, w)
				}
			}
		})
	}
}
