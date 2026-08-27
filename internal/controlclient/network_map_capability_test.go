package controlclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestSubscribeNetworkMapDeclaresCapabilities pins the §8.4 client
// side: every poll request body declares public-share-v1 so the CP can
// record it on Device.agent_capabilities and (post-B2) emit the Public
// Share map fields to this agent.
//
// The onboarding pair is CONDITIONAL (waired-agent#133), and the
// not-capable row is the product contract, not a record of today: a host
// with local AI off never builds the desired-state applier, so declaring
// the capability made the wizard offer a setup that could never proceed.
// public-share-v1 stays in both rows on purpose — the CP tells "does not
// offer onboarding" from "too old to declare anything" by whether the
// capability list is empty at all.
func TestSubscribeNetworkMapDeclaresCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capable bool
		want    []string
		absent  []string
	}{
		{
			name: "onboarding capable", capable: true,
			want: []string{
				signer.CapabilityContextWindowV1,
				signer.CapabilityRAMAvailableV1,
				signer.CapabilityRAMAvailableV2,
				// residency-v1 rides in BOTH rows and unconditionally, for
				// the reason context-window-v1 and the ram-available pair
				// do: it declares that this BUILD understands
				// InferenceState.DesiredIdleTimeout, which rides the signed
				// map — an agent that does not know it drops it on
				// canonical re-marshal and fails verification of the whole
				// map. It is NOT a statement that this host can honour a
				// keep-alive; a vLLM host declares it and still has no such
				// axis, which is what InferenceState.ResidencyUnsupported
				// says instead (waired-agent#1030).
				signer.CapabilityResidencyV1,
				signer.CapabilityPublicShareV1,
				signer.CapabilityOnboardingV1,
				signer.CapabilityOnboardingV2,
				signer.CapabilityOnboardingV3,
				signer.CapabilityOnboardingV4,
			},
		},
		{
			// context-window-v1 joins public-share-v1 in BOTH rows, and for
			// the same reason those two differ from the onboarding set: they
			// declare what this BUILD understands on the wire, not what this
			// host is configured to do. A device with local AI off still
			// consumes peer entries and still has to be able to read the
			// window on them (waired#1031).
			name: "local AI off", capable: false,
			// ram-available-v1 rides with them: it too declares what
			// this BUILD understands on peer entries (waired-agent#568).
			want: []string{
				signer.CapabilityContextWindowV1,
				signer.CapabilityRAMAvailableV1,
				// ram-available-v2 rides with v1 in BOTH rows and never
				// alone: the CP strips the whole measurement for a poller
				// that declares v2 without v1 (waired-agent#699).
				signer.CapabilityRAMAvailableV2,
				signer.CapabilityResidencyV1,
				signer.CapabilityPublicShareV1,
			},
			absent: []string{
				signer.CapabilityOnboardingV1,
				signer.CapabilityOnboardingV2,
				signer.CapabilityOnboardingV3,
				signer.CapabilityOnboardingV4,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotBody := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/network-map/poll" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				gotBody <- body
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"version":1,"network_id":"wn_test"}` + "\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer srv.Close()

			c := &Client{
				BaseURL: srv.URL, HTTP: srv.Client(),
				BearerFn:          func() string { return "tok" },
				OnboardingCapable: tc.capable,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			frames, errs := c.SubscribeNetworkMap(ctx)

			select {
			case nm := <-frames:
				if nm == nil || nm.NetworkID != "wn_test" {
					t.Fatalf("frame = %+v", nm)
				}
			case err := <-errs:
				t.Fatalf("subscribe error: %v", err)
			case <-ctx.Done():
				t.Fatalf("timeout waiting for frame")
			}

			var req struct {
				Capabilities []string `json:"capabilities"`
			}
			if err := json.Unmarshal(<-gotBody, &req); err != nil {
				t.Fatalf("poll body decode: %v", err)
			}
			declared := map[string]bool{}
			for _, c := range req.Capabilities {
				declared[c] = true
			}
			for _, want := range tc.want {
				if !declared[want] {
					t.Errorf("poll body must declare %q, got %q", want, req.Capabilities)
				}
			}
			for _, absent := range tc.absent {
				if declared[absent] {
					t.Errorf("poll body must NOT declare %q, got %q", absent, req.Capabilities)
				}
			}
		})
	}
}

// TestSubscribeNetworkMapReportsClientVersion pins the waired-agent#655
// contract: the poll body carries this build's version, so the control
// plane's record follows an in-place upgrade instead of freezing at
// whatever first enrolled.
//
// Product contract, ratified on waired-agent#655 (design comment,
// 2026-08-12): the version rides the POLL and not one of the 5s pushes,
// because the poll is opened once per daemon start and per reconnect, and
// an installer upgrade always restarts the daemon.
//
// The empty case is contract too. Omitted means "no claim", and the CP is
// required to keep whatever it already recorded — which it cannot do if an
// agent that does not know its version sends an empty string instead.
func TestSubscribeNetworkMapReportsClientVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   string
		wantKey   bool
		wantValue string
	}{
		{
			name:      "reports the build version",
			version:   "0.0.2-edge.20260812010203+abcdef1",
			wantKey:   true,
			wantValue: "0.0.2-edge.20260812010203+abcdef1",
		},
		{
			name:    "unset version is omitted, not empty",
			version: "",
			wantKey: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotBody := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				gotBody <- body
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"version":1,"network_id":"wn_test"}` + "\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer srv.Close()

			c := &Client{
				BaseURL: srv.URL, HTTP: srv.Client(),
				BearerFn:      func() string { return "tok" },
				ClientVersion: tc.version,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			frames, errs := c.SubscribeNetworkMap(ctx)
			select {
			case nm := <-frames:
				if nm == nil || nm.NetworkID != "wn_test" {
					t.Fatalf("frame = %+v", nm)
				}
			case err := <-errs:
				t.Fatalf("subscribe error: %v", err)
			case <-ctx.Done():
				t.Fatalf("timeout waiting for frame")
			}

			raw := <-gotBody
			// Decoded into a map, not a struct: a struct field cannot tell
			// "absent" from "present and empty", and that distinction is the
			// whole of the second case.
			var req map[string]any
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatalf("poll body decode: %v (body %s)", err, raw)
			}
			got, ok := req["client_version"]
			if ok != tc.wantKey {
				t.Fatalf("client_version present = %v, want %v (body %s)", ok, tc.wantKey, raw)
			}
			if tc.wantKey && got != tc.wantValue {
				t.Errorf("client_version = %v, want %q (body %s)", got, tc.wantValue, raw)
			}
			// The version must not have cost the capability declaration.
			if _, ok := req["capabilities"]; !ok {
				t.Errorf("poll body lost its capabilities (body %s)", raw)
			}
		})
	}
}
