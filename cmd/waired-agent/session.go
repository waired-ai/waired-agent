package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/internal/testharness"
)

// errNotEnrolled is returned by switchboard-delegated actions invoked
// before the daemon has an active session (no identity yet). Read-only
// providers return empty/zero views instead so the tray can render a
// clean "not signed in" state without 404-ing.
var errNotEnrolled = errors.New("waired-agent: not enrolled")

// session bundles every identity-dependent runtime handle built by the
// activation closure (the former inline body of run()). Exactly one
// session is "live" at a time, published into switchboard.cur after it
// is fully built so no management handler ever observes a half-wired
// runtime. For #177 a session is only torn down at process shutdown;
// the cancel/teardown plumbing also supports a future logout.
type session struct {
	provider      *agentProvider
	pinger        *agentPinger
	pause         *pauseManager
	infControl    *inferenceController
	shareControl  *shareController       // nil when --disable-inference
	publicShare   *publicShareController // nil when --disable-inference
	workerControl *workerController
	meshAgg       *inferencemesh.Aggregator
	infProvider   management.InferenceProvider // nil when --disable-inference
	engControl    *engineController            // nil when --disable-inference (#186 hard engine power)
	swapControl   *modelSwapController         // nil when --disable-inference (#812 in-process model swap)
	setupRec      *setupReconciler             // nil when --disable-inference (waired#835 onboarding executor lease)
	obsState      *observabilityState

	engine      *wgnet.Engine
	stateWriter *state.Writer
	dispatcher  testharness.Dispatcher
	cancel      context.CancelFunc
	wg          *sync.WaitGroup
	logger      *slog.Logger
}

// teardown stops the session's goroutines and releases its OS resources.
// Mirrors the deferred cleanup the old monolithic run() performed at
// return: cancel the session context, stop the test-harness dispatcher
// (best-effort, 5s), wait for goroutines, close the WG engine, and
// remove the runtime/state file.
func (s *session) teardown() {
	if s == nil {
		return
	}
	if s.logger != nil {
		s.logger.Debug("session teardown: begin")
	}
	s.cancel()
	sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	_ = s.dispatcher.Stop(sctx)
	c()
	s.wg.Wait()
	s.engine.Close()
	if err := s.stateWriter.Remove(); err != nil {
		s.logger.Warn("remove runtime/state file on shutdown", "err", err)
	}
	if s.logger != nil {
		s.logger.Debug("session teardown: complete")
	}
}

// switchboard owns the at-most-one live session and implements the
// management provider interfaces by delegating to it. A nil session
// (the resting state of a fresh, unenrolled daemon) yields unenrolled
// views / errNotEnrolled. The management server is constructed once at
// boot wired to the switchboard, so a login that completes at runtime
// activates the daemon live by publishing a session — no restart, no
// re-registration of routes.
//
// Several management interfaces collide on method names (StatusProvider
// and InferenceProvider both have Status; InferenceController,
// ShareController and WorkerController all have State), so those are
// served by the small sb* adapter types below rather than directly on
// *switchboard.
type switchboard struct {
	cur atomic.Pointer[session]
}

func (sb *switchboard) current() *session { return sb.cur.Load() }

// publish installs a fully-built session as the live one. Uses CAS so a
// double-activation (boot race with a concurrent login) can never
// clobber an existing session; the loser's caller is expected to tear
// its half down.
func (sb *switchboard) publish(s *session) bool {
	ok := sb.cur.CompareAndSwap(nil, s)
	if s.logger != nil {
		s.logger.Debug("session publish", "published", ok)
	}
	return ok
}

// reset clears the live session pointer back to nil so a subsequent
// activate() can publish a fresh session. Used by the Node Key rotation
// re-activation path (#228): after the old session is torn down, reset
// lets activate() rebuild every component (engine, multiplex-bind, relay
// factory, disco) with the freshly rotated key loaded from disk. The
// caller must have already torn down the previous session.
func (sb *switchboard) reset() { sb.cur.Store(nil) }

// --- StatusProvider / IdentityProvider / Pinger / PauseController /
//     InferenceMeshProvider / ObservabilityStateProvider: no method-name
//     collisions, so implemented directly on *switchboard. ---

func (sb *switchboard) Status() management.Status {
	if s := sb.current(); s != nil {
		return s.provider.Status()
	}
	return management.Status{}
}

func (sb *switchboard) Identity() management.IdentityView {
	if s := sb.current(); s != nil {
		return s.provider.Identity()
	}
	return management.IdentityView{Enrolled: false}
}

func (sb *switchboard) PingPeer(ctx context.Context, peer string) (management.PingResult, error) {
	if s := sb.current(); s != nil {
		return s.pinger.PingPeer(ctx, peer)
	}
	return management.PingResult{}, errNotEnrolled
}

func (sb *switchboard) Pause(ctx context.Context) error {
	if s := sb.current(); s != nil {
		return s.pause.Pause(ctx)
	}
	return errNotEnrolled
}

func (sb *switchboard) Resume(ctx context.Context) error {
	if s := sb.current(); s != nil {
		return s.pause.Resume(ctx)
	}
	return errNotEnrolled
}

func (sb *switchboard) Phase() (current, desired state.Phase) {
	if s := sb.current(); s != nil {
		return s.pause.Phase()
	}
	return "", ""
}

func (sb *switchboard) Snapshot() inferencemesh.Snapshot {
	if s := sb.current(); s != nil {
		return s.meshAgg.Snapshot()
	}
	return inferencemesh.Snapshot{}
}

func (sb *switchboard) ObservabilityState() management.ObservabilityState {
	if s := sb.current(); s != nil {
		return s.obsState.ObservabilityState()
	}
	return management.ObservabilityState{}
}

// --- Adapters for the collision interfaces. ---

type sbInfControl struct{ sb *switchboard }

func (a sbInfControl) Enable(ctx context.Context) error {
	if s := a.sb.current(); s != nil {
		return s.infControl.Enable(ctx)
	}
	return errNotEnrolled
}

func (a sbInfControl) Disable(ctx context.Context) error {
	if s := a.sb.current(); s != nil {
		return s.infControl.Disable(ctx)
	}
	return errNotEnrolled
}

func (a sbInfControl) State() (current, desired state.InferenceState) {
	if s := a.sb.current(); s != nil {
		return s.infControl.State()
	}
	return "", ""
}

type sbShareControl struct{ sb *switchboard }

func (a sbShareControl) Share(ctx context.Context) error {
	if s := a.sb.current(); s != nil && s.shareControl != nil {
		return s.shareControl.Share(ctx)
	}
	return errNotEnrolled
}

func (a sbShareControl) Unshare(ctx context.Context) error {
	if s := a.sb.current(); s != nil && s.shareControl != nil {
		return s.shareControl.Unshare(ctx)
	}
	return errNotEnrolled
}

func (a sbShareControl) State() (current, desired state.ShareMeshState) {
	if s := a.sb.current(); s != nil && s.shareControl != nil {
		return s.shareControl.State()
	}
	return "", ""
}

type sbPublicShareControl struct{ sb *switchboard }

func (a sbPublicShareControl) Enable(ctx context.Context, maxClients int) (management.PublicShareResult, error) {
	if s := a.sb.current(); s != nil && s.publicShare != nil {
		return s.publicShare.Enable(ctx, maxClients)
	}
	return management.PublicShareResult{}, errNotEnrolled
}

func (a sbPublicShareControl) Disable(ctx context.Context) (management.PublicShareResult, error) {
	if s := a.sb.current(); s != nil && s.publicShare != nil {
		return s.publicShare.Disable(ctx)
	}
	return management.PublicShareResult{}, errNotEnrolled
}

func (a sbPublicShareControl) State() (current, desired state.PublicShareState) {
	if s := a.sb.current(); s != nil && s.publicShare != nil {
		return s.publicShare.State()
	}
	return "", ""
}

func (a sbPublicShareControl) Synced() bool {
	if s := a.sb.current(); s != nil && s.publicShare != nil {
		return s.publicShare.Synced()
	}
	return true
}

func (a sbPublicShareControl) MaxClients() int {
	if s := a.sb.current(); s != nil && s.publicShare != nil {
		return s.publicShare.MaxClients()
	}
	return 0
}

type sbWorkerControl struct{ sb *switchboard }

func (a sbWorkerControl) SetMode(ctx context.Context, mode state.RoutingMode) error {
	if s := a.sb.current(); s != nil {
		return s.workerControl.SetMode(ctx, mode)
	}
	return errNotEnrolled
}

func (a sbWorkerControl) SetPin(ctx context.Context, peerDeviceID string) error {
	if s := a.sb.current(); s != nil {
		return s.workerControl.SetPin(ctx, peerDeviceID)
	}
	return errNotEnrolled
}

func (a sbWorkerControl) Clear(ctx context.Context) error {
	if s := a.sb.current(); s != nil {
		return s.workerControl.Clear(ctx)
	}
	return errNotEnrolled
}

func (a sbWorkerControl) State() (current, desired state.RoutingPreference) {
	if s := a.sb.current(); s != nil {
		return s.workerControl.State()
	}
	return state.RoutingPreference{}, state.RoutingPreference{}
}

type sbEngineControl struct{ sb *switchboard }

func (a sbEngineControl) StopEngine(ctx context.Context) error {
	if s := a.sb.current(); s != nil && s.engControl != nil {
		return s.engControl.StopEngine(ctx)
	}
	return errNotEnrolled
}

func (a sbEngineControl) StartEngine(ctx context.Context) error {
	if s := a.sb.current(); s != nil && s.engControl != nil {
		return s.engControl.StartEngine(ctx)
	}
	return errNotEnrolled
}

func (a sbEngineControl) EngineState() (management.EnginePowerState, bool) {
	if s := a.sb.current(); s != nil && s.engControl != nil {
		return s.engControl.EngineState()
	}
	return "", false
}

type sbModelSwapControl struct{ sb *switchboard }

// ApplyModelSwitch delegates the #812 in-process preferred-model switch to the
// live session's controller. Returns errNotEnrolled (which the handler treats
// as "fall back to the supervised restart") when no session is enrolled yet.
func (a sbModelSwapControl) ApplyModelSwitch(ctx context.Context, modelID string) (bool, error) {
	if s := a.sb.current(); s != nil && s.swapControl != nil {
		return s.swapControl.ApplyModelSwitch(ctx, modelID)
	}
	return false, errNotEnrolled
}

// sbSetupExecutor delegates the onboarding executor lease (waired#835
// §9/§11) to the live session's reconciler. The management server is
// built once at boot but the reconciler only exists after enrollment, so
// the routes stay registered and answer with the zero state until a
// session is live — an executor polling before enrollment sees
// active=false and simply keeps waiting, which is the same answer it
// gets from an enrolled host where nobody has started setup yet.
type sbSetupExecutor struct{ sb *switchboard }

func (a sbSetupExecutor) liveOrNil() *setupReconciler {
	if s := a.sb.current(); s != nil {
		return s.setupRec
	}
	return nil
}

func (a sbSetupExecutor) SetupState(ctx context.Context) management.SetupStateResponse {
	return a.liveOrNil().SetupState(ctx)
}

func (a sbSetupExecutor) NoteExecutor(ctx context.Context, req management.SetupExecutorRequest) management.SetupStateResponse {
	return a.liveOrNil().NoteExecutor(ctx, req)
}

type sbInfProvider struct{ sb *switchboard }

func (a sbInfProvider) liveOrNil() management.InferenceProvider {
	if s := a.sb.current(); s != nil {
		return s.infProvider
	}
	return nil
}

func (a sbInfProvider) Status(ctx context.Context) management.InferenceStatus {
	if p := a.liveOrNil(); p != nil {
		return p.Status(ctx)
	}
	return management.InferenceStatus{}
}

func (a sbInfProvider) Hardware(ctx context.Context) hardware.Profile {
	if p := a.liveOrNil(); p != nil {
		return p.Hardware(ctx)
	}
	return hardware.Profile{}
}

func (a sbInfProvider) Runtimes(ctx context.Context) []management.RuntimeStatus {
	if p := a.liveOrNil(); p != nil {
		return p.Runtimes(ctx)
	}
	return nil
}

func (a sbInfProvider) ListModels(ctx context.Context) []management.ModelEntry {
	if p := a.liveOrNil(); p != nil {
		return p.ListModels(ctx)
	}
	return nil
}

func (a sbInfProvider) PullModel(ctx context.Context, modelOrAlias string) (management.PullJob, error) {
	if p := a.liveOrNil(); p != nil {
		return p.PullModel(ctx, modelOrAlias)
	}
	return management.PullJob{}, errNotEnrolled
}

func (a sbInfProvider) DeleteModel(ctx context.Context, modelID string) error {
	if p := a.liveOrNil(); p != nil {
		return p.DeleteModel(ctx, modelID)
	}
	return errNotEnrolled
}

func (a sbInfProvider) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	if p := a.liveOrNil(); p != nil {
		return p.Select(ctx, req)
	}
	return router.Selection{}, errNotEnrolled
}

func (a sbInfProvider) RunBenchmark(ctx context.Context) (management.BenchmarkOutcome, bool, error) {
	if p := a.liveOrNil(); p != nil {
		return p.RunBenchmark(ctx)
	}
	return management.BenchmarkOutcome{}, false, errNotEnrolled
}

func (a sbInfProvider) DismissRecommendation(from, to string) error {
	if p := a.liveOrNil(); p != nil {
		return p.DismissRecommendation(from, to)
	}
	return errNotEnrolled
}

func (a sbInfProvider) BenchmarkStatus() management.BenchmarkStatusResponse {
	if p := a.liveOrNil(); p != nil {
		return p.BenchmarkStatus()
	}
	return management.BenchmarkStatusResponse{State: management.BenchmarkStateIdle}
}
