package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
)

// TestDeviceKeyFindingFrom pins what each verdict says.
//
// A fix pin, not a record of today's behaviour: waired-ai/waired#1137 is
// a host whose overlay was dead in both directions while `waired doctor`
// reported nothing at all.
func TestDeviceKeyFindingFrom(t *testing.T) {
	cases := []struct {
		name       string
		agreement  string
		wantStatus integration.Status
		wantQuiet  bool
	}{
		{
			name:       "a key the network does not agree with fails the run",
			agreement:  management.NodeKeyAgreementDiverged,
			wantStatus: integration.StatusFail,
		},
		{
			name:       "a rotation in flight is reported, but is not a fault",
			agreement:  management.NodeKeyAgreementRotating,
			wantStatus: integration.StatusSkip,
		},
		{
			name:      "agreement says nothing",
			agreement: management.NodeKeyAgreementOK,
			wantQuiet: true,
		},
		{
			name:      "no verdict yet says nothing",
			agreement: "",
			wantQuiet: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deviceKeyFindingFrom(tc.agreement)
			if tc.wantQuiet {
				if got.Subject != "" {
					t.Fatalf("deviceKeyFindingFrom(%q) = %+v, want silence", tc.agreement, got)
				}
				return
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("deviceKeyFindingFrom(%q).Status = %v, want %v", tc.agreement, got.Status, tc.wantStatus)
			}
			if got.Subject != "device key" {
				t.Fatalf("Subject = %q, want %q", got.Subject, "device key")
			}
		})
	}
}

// TestDeviceKeyFinding_ReadsTheDaemon covers the fetch: the verdict is
// only useful if it comes off the live daemon, and a doctor that could
// not reach it must stay quiet rather than accuse the device.
func TestDeviceKeyFinding_ReadsTheDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(management.Status{
			DeviceName:       "workshop-mac",
			NodeKeyAgreement: management.NodeKeyAgreementDiverged,
		})
	}))
	defer srv.Close()

	got := deviceKeyFinding(context.Background(), srv.URL)
	if got.Status != integration.StatusFail || got.Subject != "device key" {
		t.Fatalf("deviceKeyFinding = %+v, want a failing device key finding", got)
	}

	srv.Close()
	if quiet := deviceKeyFinding(context.Background(), srv.URL); quiet.Subject != "" {
		t.Fatalf("with the daemon down, deviceKeyFinding = %+v, want silence", quiet)
	}
}

// TestCollectDoctorFindings_ReportsADivergedKey drives the check through
// the collector the command actually calls, so a finding that never got
// appended would fail here.
func TestCollectDoctorFindings_ReportsADivergedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(management.Status{
			NodeKeyAgreement: management.NodeKeyAgreementDiverged,
		})
	}))
	defer srv.Close()

	findings := collectDoctorFindings(t.Context(), t.TempDir(), t.TempDir(),
		"http://127.0.0.1:65535", srv.URL, trayDoctor{}, servicediag.Result{})

	var found bool
	for _, f := range findings {
		if f.Subject == "device key" {
			found = true
			if f.Status != integration.StatusFail {
				t.Fatalf("device key finding = %v, want StatusFail", f.Status)
			}
		}
	}
	if !found {
		t.Fatalf("collectDoctorFindings dropped the device key finding: %+v", findings)
	}
}
