package main

import (
	"context"
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
type localAdmissionRelay struct {
	srv atomic.Pointer[inference.Server]
}

// Set publishes the session's inference server. Called once during
// session wiring.
func (r *localAdmissionRelay) Set(s *inference.Server) { r.srv.Store(s) }

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
