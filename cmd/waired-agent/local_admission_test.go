package main

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inference"
)

// stubGatewayHandler is the minimum inference.Config.GatewayHandler
// needed to get a full (non-ping-only) Server, which is what carries
// the admission counter.
type stubGatewayHandler struct{}

func (stubGatewayHandler) Handler() http.Handler { return http.NotFoundHandler() }

// TestLocalAdmissionRelay_NoopBeforeSet: the local listeners accept
// requests during the boot window before the session publishes the
// inference server. Admit must be safe (and free) there.
func TestLocalAdmissionRelay_NoopBeforeSet(t *testing.T) {
	var relay localAdmissionRelay
	release := relay.Admit(context.Background())
	if release == nil {
		t.Fatal("Admit must always return a non-nil release")
	}
	release()
}

// TestLocalAdmissionRelay_DelegatesAfterSet: once the session wires the
// server, the owner's local work lands on the shared admission counter
// — that is what raises the owner-priority latch (spec §8.2).
func TestLocalAdmissionRelay_DelegatesAfterSet(t *testing.T) {
	srv := inference.NewServerWithConfig(inference.Config{
		DeviceName:     "dev-self",
		GatewayHandler: stubGatewayHandler{},
		Capacity:       2,
	})
	var relay localAdmissionRelay
	relay.Set(srv)

	release := relay.Admit(context.Background())
	if got := srv.InflightCount(); got != 1 {
		t.Fatalf("inflight after Admit: got %d, want 1", got)
	}
	release()
	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after release: got %d, want 0", got)
	}
}

// TestLocalAdmissionRelay_SetRacesWithAdmit: the listeners are already
// serving when Set runs, so the two must not race (this test is the
// -race detector's hook).
func TestLocalAdmissionRelay_SetRacesWithAdmit(t *testing.T) {
	srv := inference.NewServerWithConfig(inference.Config{
		DeviceName:     "dev-self",
		GatewayHandler: stubGatewayHandler{},
		Capacity:       4,
	})
	var relay localAdmissionRelay

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relay.Set(srv)
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			relay.Admit(context.Background())()
		}
	}()
	wg.Wait()

	if got := srv.InflightCount(); got != 0 {
		t.Fatalf("inflight after every release: got %d, want 0", got)
	}
}

// PRODUCT CONTRACT (waired-agent#703): the relay reads the counter as well
// as feeding it, and answers 0 before Set.
//
// The host-speed measurement lives on the provider, which is built before
// the inference server exists — the reason this type exists at all. Before
// Set nothing is serving, which is the same premise Admit's no-op rests
// on; a relay that panicked or lied there would take the quiet gate with
// it on every boot.
func TestLocalAdmissionRelay_ReportsWhatItIsFeeding(t *testing.T) {
	var relay localAdmissionRelay

	if got := relay.InflightCount(); got != 0 {
		t.Errorf("InflightCount before Set = %d, want 0", got)
	}
	if got := relay.AdmittedCount(); got != 0 {
		t.Errorf("AdmittedCount before Set = %d, want 0", got)
	}

	srv := inference.NewServerWithConfig(inference.Config{
		DeviceName:     "dev-self",
		GatewayHandler: stubGatewayHandler{},
		Capacity:       4,
	})
	relay.Set(srv)

	release := relay.Admit(context.Background())
	if got := relay.InflightCount(); got != 1 {
		t.Errorf("InflightCount while serving = %d, want 1", got)
	}
	release()
	if got := relay.InflightCount(); got != 0 {
		t.Errorf("InflightCount after release = %d, want 0", got)
	}
	// The whole request still happened, and the cumulative counter is the
	// only thing that can still say so.
	if got := relay.AdmittedCount(); got != 1 {
		t.Errorf("AdmittedCount after a finished request = %d, want 1", got)
	}
}
