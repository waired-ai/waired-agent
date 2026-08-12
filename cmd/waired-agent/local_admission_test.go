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

// capacityRecorder captures every admission ceiling the inference server was
// actually given. inference.Recorder.SetCapacity fires at construction and on
// every Server.SetCapacity, which makes it the seam for watching the relay's
// decisions land on a REAL server instead of reading the relay's own fields
// — the fields are an implementation detail, the applied ceiling is the
// product behaviour.
type capacityRecorder struct {
	mu      sync.Mutex
	applied []int
}

func (c *capacityRecorder) RecordServed(string, uint32) {}
func (c *capacityRecorder) SetInflight(int)             {}

func (c *capacityRecorder) SetCapacity(total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = append(c.applied, total)
}

// enforced is the ceiling in effect now: the last value the server was
// given. -1 when it was never given one, which cannot happen — construction
// always reports.
func (c *capacityRecorder) enforced() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) == 0 {
		return -1
	}
	return c.applied[len(c.applied)-1]
}

func newFloorServer(rec *capacityRecorder) *inference.Server {
	return inference.NewServerWithConfig(inference.Config{
		DeviceName:     "dev-self",
		GatewayHandler: stubGatewayHandler{},
		Capacity:       unmeasuredCapacity,
		Recorder:       rec,
	})
}

// TestLocalAdmissionRelay_OnlyAPositiveCapacityMovesTheCeiling pins the
// PRODUCT CONTRACT from waired-agent#738 and the owner's ruling of
// 2026-08-13: the overlay listener enforces one request at a time until a
// real figure arrives, and only a POSITIVE figure moves the ceiling.
//
// Two sources retune it and neither can see the other. The boot benchmark
// finishes inside the probe goroutine — minutes after the listener exists on
// a first install, where the reported measurement ran six minutes in. The
// network map lands in applySelf within seconds, but the figure it serves is
// an echo of what this agent published, so it reads 0 for that whole window.
//
// Passing that 0 through is what left the listener unbounded: inflightCounter
// reads 0 as unlimited, and the requesting side reads an advertised 0 the
// same way (the router's admission pre-filter only drops a peer whose
// capacity is > 0, and probe_client only calls one full when total > 0). So
// the map's zero is dropped, its positive figure wins for good — the control
// plane folds the admin per-device override into that one — and a benchmark
// landing after it does not undo it.
func TestLocalAdmissionRelay_OnlyAPositiveCapacityMovesTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		// steps runs the sequence under test. Each case calls Set where it
		// belongs, because WHEN the session publishes the server relative
		// to the two sources is part of what is being pinned.
		steps func(r *localAdmissionRelay, srv *inference.Server)
		want  int
	}{
		{
			name:  "nothing has spoken yet",
			steps: func(r *localAdmissionRelay, srv *inference.Server) { r.Set(srv) },
			want:  unmeasuredCapacity,
		},
		{
			name: "the benchmark measured this host",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				r.Set(srv)
				r.SeedCapacity(3)
			},
			want: 3,
		},
		{
			name: "the benchmark finished before the listener was published",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				// A warm boot: the bench cache hit can land before the
				// session wires the server.
				r.SeedCapacity(3)
				r.Set(srv)
			},
			want: 3,
		},
		{
			name: "the map served a figure",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				r.Set(srv)
				r.SetCapacityFromMap(8)
			},
			want: 8,
		},
		{
			name: "the map echoes the zero of a host that has not measured",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				r.Set(srv)
				r.SetCapacityFromMap(0)
			},
			want: unmeasuredCapacity,
		},
		{
			name: "the map still echoes zero after the benchmark measured",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				r.Set(srv)
				r.SeedCapacity(3)
				// The publish has not reached the control plane yet, so the
				// next frames carry the old zero. They must not undo the
				// measurement.
				r.SetCapacityFromMap(0)
				r.SetCapacityFromMap(0)
			},
			want: 3,
		},
		{
			name: "a late benchmark does not undo what the control plane served",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				r.Set(srv)
				r.SetCapacityFromMap(8)
				// The admin's per-device override is folded into the served
				// figure; a local guess arriving afterwards must not win.
				r.SeedCapacity(3)
			},
			want: 8,
		},
		{
			name: "a skipped benchmark does not move the ceiling",
			steps: func(r *localAdmissionRelay, srv *inference.Server) {
				r.Set(srv)
				// RunBootBenchmark returns 0 for a host with no engine at
				// all, where 0 means "no admission cap" on purpose.
				r.SeedCapacity(0)
			},
			want: unmeasuredCapacity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &capacityRecorder{}
			srv := newFloorServer(rec)
			var relay localAdmissionRelay
			tc.steps(&relay, srv)

			if got := rec.enforced(); got != tc.want {
				t.Errorf("enforced ceiling = %d, want %d (applied: %v)", got, tc.want, rec.applied)
			}
			if rec.enforced() == 0 {
				t.Error("a ceiling of 0 is unlimited: this host would accept " +
					"unbounded concurrent peer requests (waired-agent#738)")
			}
		})
	}
}

// The two sources run on different goroutines — the probe goroutine and the
// map loop — so the resolution has to hold under -race, and the map's figure
// has to be the answer whichever order they land in.
func TestLocalAdmissionRelay_CapacitySourcesReachTheSameAnswer(t *testing.T) {
	for range 20 {
		rec := &capacityRecorder{}
		srv := newFloorServer(rec)
		var relay localAdmissionRelay
		relay.Set(srv)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			relay.SeedCapacity(3)
		}()
		go func() {
			defer wg.Done()
			relay.SetCapacityFromMap(8)
		}()
		wg.Wait()

		if got := rec.enforced(); got != 8 {
			t.Fatalf("enforced ceiling = %d, want 8 whichever order the two "+
				"sources landed in (applied: %v)", got, rec.applied)
		}
	}
}
