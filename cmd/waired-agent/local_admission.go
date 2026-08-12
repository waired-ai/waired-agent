package main

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/inference"
)

// localAdmissionRelay bridges a boot-order gap: the LOCAL gateway
// surfaces (loopback :9473, Claude intercept :9472, data plane :9479) are
// built inside startInferenceSubsystem, while the inference.Server that
// owns the shared admission counter is constructed later in the session
// goroutine. The surfaces get Admit at construction time and start
// counting the owner's local engine work the moment the session
// publishes the server (Set).
//
// Before Set — the few hundred milliseconds of boot before the overlay
// server exists — Admit is a no-op. Nothing is serving public or peer
// traffic yet in that window, so there is no owner-priority decision to
// get wrong.
//
// It also owns the listener's admission ceiling, because two sources
// retune it and neither can see the other: the boot benchmark, which
// finishes inside the probe goroutine, and the network map, which lands in
// applySelf. Resolving them here rather than at the two call sites is what
// keeps a benchmark that finishes late from overwriting a figure the
// control plane has already served (waired-agent#738).
type localAdmissionRelay struct {
	srv atomic.Pointer[inference.Server]

	// capMu guards the capacity resolution below. A mutex rather than more
	// atomics: the decision reads two fields and writes both, and it runs
	// twice at boot and once per map frame — never on a request path.
	capMu sync.Mutex
	// capNow is the ceiling currently applied, or held for a Set that has
	// not happened yet. 0 means "nothing has said anything".
	capNow int
	// mapSaid records that the network map has served a positive capacity.
	// From then on the benchmark no longer speaks: the served figure has
	// the admin's per-device override folded into it, and a local guess
	// must not undo that.
	mapSaid bool
}

// Set publishes the session's inference server. Called once during
// session wiring, and applies whatever ceiling has been decided so far —
// the boot benchmark can finish before this runs.
func (r *localAdmissionRelay) Set(s *inference.Server) {
	r.srv.Store(s)
	r.capMu.Lock()
	defer r.capMu.Unlock()
	if r.capNow > 0 && s != nil {
		s.SetCapacity(r.capNow)
	}
}

// SeedCapacity applies the boot benchmark's figure. Ignored once the
// network map has served one, and ignored for a non-positive figure: the
// skip paths in RunBootBenchmark return 0 for a host with no engine at all,
// and that host's "no ceiling" is deliberate.
func (r *localAdmissionRelay) SeedCapacity(n int) {
	if n <= 0 {
		return
	}
	r.capMu.Lock()
	defer r.capMu.Unlock()
	if r.mapSaid {
		return
	}
	r.capNow = n
	if s := r.srv.Load(); s != nil {
		s.SetCapacity(n)
	}
}

// SetCapacityFromMap applies the capacity the network map served, which is
// authoritative: the control plane folds the admin per-device max-clients
// override into it.
//
// A ZERO is not passed through. inference.Server reads 0 as unlimited, and
// the served figure is an echo of what this agent published — so before the
// boot benchmark has measured anything, every frame carries 0, and passing
// that through would hand a host that has not measured itself an unbounded
// ceiling for as long as the measurement takes. That is waired-agent#738
// itself, and it is why the floor could not simply be seeded at
// construction and left alone.
//
// The cost is that an admin who wants unlimited cannot express it, because
// "unlimited" and "no figure yet" are the same value on the wire. Recorded
// as today's behaviour rather than asserted as right; whether unlimited
// needs its own encoding is a control-plane question, filed separately.
func (r *localAdmissionRelay) SetCapacityFromMap(n int) {
	if n <= 0 {
		return
	}
	r.capMu.Lock()
	defer r.capMu.Unlock()
	r.mapSaid = true
	r.capNow = n
	if s := r.srv.Load(); s != nil {
		s.SetCapacity(n)
	}
}

// Admit is the gateway.Deps.LocalAdmission hook. The returned release
// is always non-nil.
func (r *localAdmissionRelay) Admit(ctx context.Context) func() {
	if s := r.srv.Load(); s != nil {
		return s.AdmitLocal(ctx)
	}
	return func() {}
}

// InflightCount reports what this machine is serving right now, and
// AdmittedCount how much it has served in total. Both read the same
// counter Admit feeds, through the same relay, so the answer covers peer
// traffic and the owner's own local work alike.
//
// They are here rather than read off inference.Server directly for the
// reason the type exists: the host-speed measurement lives on the
// provider, which is built before the server (waired-agent#703). Before
// Set they answer 0 — nothing is serving in that window, which is the
// same premise Admit's no-op rests on.
func (r *localAdmissionRelay) InflightCount() int {
	if s := r.srv.Load(); s != nil {
		return s.InflightCount()
	}
	return 0
}

func (r *localAdmissionRelay) AdmittedCount() uint64 {
	if s := r.srv.Load(); s != nil {
		return s.AdmittedCount()
	}
	return 0
}
