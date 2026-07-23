package main

import (
	"context"
	"sync/atomic"

	"github.com/waired-ai/waired-agent/internal/inference"
)

// localAdmissionRelay bridges a boot-order gap: the LOCAL gateway
// surfaces (loopback :9473, Claude intercept :9472, OpenCode :9480) are
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
