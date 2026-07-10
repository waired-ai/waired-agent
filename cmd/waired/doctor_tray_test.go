package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

func TestTrayFindingFromResult(t *testing.T) {
	tests := []struct {
		name        string
		in          trayhost.Result
		wantStatus  integration.Status
		wantSubject string // "" means an empty (skipped) finding
		wantDetail  string // "" means don't assert detail
	}{
		{
			name:        "host present is OK",
			in:          trayhost.Result{Status: trayhost.HostPresent},
			wantStatus:  integration.StatusOK,
			wantSubject: "system tray host",
		},
		{
			name:        "no host warns with the hint as detail",
			in:          trayhost.Result{Status: trayhost.NoHost, Hint: "install the extension"},
			wantStatus:  integration.StatusWarn,
			wantSubject: "system tray host",
			wantDetail:  "install the extension",
		},
		{
			name:        "unsupported (MATE) warns with the hint",
			in:          trayhost.Result{Status: trayhost.Unsupported, Hint: "MATE can't render SNI"},
			wantStatus:  integration.StatusWarn,
			wantSubject: "system tray host",
			wantDetail:  "MATE can't render SNI",
		},
		{
			name:        "not applicable yields an empty (skipped) finding",
			in:          trayhost.Result{Status: trayhost.NotApplicable},
			wantSubject: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := trayFindingFromResult(tc.in)
			if got.Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", got.Subject, tc.wantSubject)
			}
			if tc.wantSubject == "" {
				return // empty finding: status/detail irrelevant
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v", got.Status, tc.wantStatus)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", got.Detail, tc.wantDetail)
			}
		})
	}
}
