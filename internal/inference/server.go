// Package inference exposes the overlay-side HTTP service that remote
// peers reach over the WireGuard data plane.
//
// Two surfaces share the same listener:
//
//   - /waired/v1/ping — anonymous diagnostic endpoint, returns
//     {ok, device, time}. Reachable from any peer in the NetworkMap;
//     used by the agent's PingPeer probe and by ad-hoc reachability
//     tests.
//
//   - /v1/* and /anthropic/v1/* — Phase 4 peer-engine inference routes,
//     mounted only when Config.GatewayHandler is non-nil. These delegate
//     to a gateway.HandlerSet (the same routes the loopback gateway
//     serves) but with the peer-auth middleware stack:
//
//     wgPeerOnly → verifyPeerSignature → pausedGate → inferenceGate
//
//     The Selector behind the gateway.HandlerSet is wired with
//     MeshSnapshotFn=nil so a peer-side request never recurses to a
//     third peer — loop prevention is built into the routing layer.
package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server holds the overlay-side HTTP routes. Construct via NewServer
// (ping-only) or NewServerWithConfig (peer-engine routing enabled).
type Server struct {
	deviceName string

	// gw, when non-nil, contributes the inference route surface
	// (handlers from internal/gateway). Wrapped in the peer-auth
	// middleware stack via Handler().
	gw          gatewayHandlerSet
	peerLookup  PeerLookup
	nonces      nonceCache
	skewWindow  time.Duration
	nonceTTL    time.Duration
	now         func() time.Time
	maxBodySize int64

	pausedGate    func(http.Handler) http.Handler
	inferenceGate func(http.Handler) http.Handler
	shareGate     func(http.Handler) http.Handler
	capacityGate  func(http.Handler) http.Handler

	// Phase 8: the operator-gate closures and inflight counter are
	// hoisted alongside the gate wrappers so the /healthz endpoint
	// (which deliberately bypasses the gates) can report current
	// state. handleHealthz reads these directly.
	isPausedFn      func() bool
	isShareDeniedFn func() bool
	inflight        *inflightCounter
	engineReadyFn   func() (bool, string)
	recorder        Recorder
}

// inflightCounter is the atomic state behind capacityGate. capacity 0
// means "unlimited"; in that case Acquire skips the CAS loop entirely
// to keep the fast path lock-free. capacity is itself atomic so the
// control plane can retune it live (Server.SetCapacity) when an admin
// changes the per-device max-clients cap — no listener restart needed.
type inflightCounter struct {
	n        atomic.Int32
	capacity atomic.Int32
}

// newInflightCounter returns a counter with the given admission ceiling
// (<= 0 ⇒ unlimited). Safe for concurrent use immediately.
func newInflightCounter(capacity int) *inflightCounter {
	c := &inflightCounter{}
	c.setCapacity(capacity)
	return c
}

// setCapacity retunes the admission ceiling live. n <= 0 ⇒ unlimited.
// Acquire reads the value atomically on its next call, so a lowered cap
// applies to new requests immediately while in-flight requests drain
// naturally (the counter is never force-decremented).
func (c *inflightCounter) setCapacity(n int) {
	if n < 0 {
		n = 0
	}
	c.capacity.Store(int32(n))
}

func (c *inflightCounter) Acquire() bool {
	capacity := int(c.capacity.Load())
	if capacity <= 0 {
		c.n.Add(1)
		return true
	}
	for {
		cur := c.n.Load()
		if int(cur) >= capacity {
			return false
		}
		if c.n.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (c *inflightCounter) Release() {
	c.n.Add(-1)
}

// InFlight reports the current concurrent-request count. Exposed for
// tests and for future metrics.
func (c *inflightCounter) InFlight() int32 { return c.n.Load() }

// InflightCount returns the agent's currently-serving peer-overlay
// request count, or 0 if Capacity was 0 (admission disabled). Wired
// from main.go into the /waired/v1/observability/state handler.
func (s *Server) InflightCount() int {
	if s.inflight == nil {
		return 0
	}
	return int(s.inflight.InFlight())
}

// gatewayHandlerSet is the local interface satisfied by
// *gateway.HandlerSet. Declared here as a small surface to avoid an
// import cycle and to keep the test surface narrow.
type gatewayHandlerSet interface {
	Handler() http.Handler
}

// nonceCache is the local alias for signedreq.NonceCache. Re-declared
// here so callers can pass any compatible cache without importing
// signedreq directly.
type nonceCache interface {
	Consume(deviceID, nonce string, now time.Time, ttl time.Duration) bool
}

// PingResponse is the JSON body of GET /waired/v1/ping.
type PingResponse struct {
	OK     bool   `json:"ok"`
	Device string `json:"device"`
	Time   string `json:"time"`
}

// NewServer returns a ping-only overlay server. Existing callers (e2e
// tests, wgnet integration test) use this form when they don't need
// the Phase 4 inference routes.
func NewServer(deviceName string) *Server {
	return &Server{
		deviceName: deviceName,
		now:        time.Now,
	}
}

// Config configures a full overlay server with peer-engine inference
// routes mounted alongside the ping endpoint.
type Config struct {
	DeviceName string

	// GatewayHandler exposes the OpenAI / Anthropic routes. When nil,
	// only /waired/v1/ping is mounted (= ping-only).
	GatewayHandler interface{ Handler() http.Handler }

	// PeerLookup resolves a WG-source overlay IP to the peer's
	// DeviceID + MachinePublicKey. Required when GatewayHandler is
	// non-nil; ignored otherwise (ping is anonymous).
	PeerLookup PeerLookup

	// NonceCache is the replay-detection store for verifyPeerSignature.
	// nil disables replay rejection (suitable only for tests).
	NonceCache nonceCache

	// SkewWindow caps the allowed clock drift on X-Waired-Issued-At.
	// 0 → DefaultSkewWindow.
	SkewWindow time.Duration

	// NonceTTL is how long a consumed nonce is remembered. 0 →
	// DefaultNonceTTL.
	NonceTTL time.Duration

	// MaxBodySize caps inbound request bodies. 0 → DefaultMaxBodyBytes.
	MaxBodySize int64

	// IsPaused / IsInferenceDisabled mirror the loopback gateway's
	// gates. Wiring the overlay listener to the same hooks means a
	// `waired pause` or `waired inference disable` rejects in-flight
	// peer requests with the same 503 + JSON error as a local
	// invocation would see.
	IsPaused            func() bool
	IsInferenceDisabled func() bool

	// IsShareDenied returns true when the operator has opted this
	// agent out of mesh-share (Phase 6). When non-nil and returning
	// true, the overlay listener rejects peer requests with 503
	// waired_inference_not_shared even though signature verification
	// passes. Composes after IsPaused / IsInferenceDisabled so the
	// most-specific error wins:
	//
	//   paused          → waired_paused
	//   inference off   → waired_inference_disabled
	//   share off       → waired_inference_not_shared
	//   overloaded      → waired_inference_overloaded
	//
	// Paired with the push-skip in the inference probe loop for
	// defense in depth: peers see a stale snapshot for at most the
	// 15s aggregator window, the listener-side 503 catches the gap.
	IsShareDenied func() bool

	// Capacity bounds the number of concurrent peer-overlay inference
	// requests this agent will admit before returning 503
	// waired_inference_overloaded. Read once at server construction
	// from the boot token/s benchmark (see Phase 7 plan §5). 0 means
	// "unlimited", which is both the backward-compat default for
	// agents that predate the field and the explicit semantics for
	// external (openai-compat) endpoints — the upstream provider
	// already does its own rate limiting in that path.
	Capacity int

	// EngineReadyFn reports whether the local inference engine is up
	// and which catalog ModelID is currently active. Wired in Phase 8
	// to power the /waired/v1/inference/healthz body so the remote
	// probe client can distinguish "engine still booting" from "peer
	// is reachable but model loading". nil disables EngineReady /
	// ModelID in the response — they read as false / "" in that case.
	EngineReadyFn func() (bool, string)

	// Recorder receives Phase 9 telemetry from the overlay listener:
	// RecordServed at every served-request termination, SetInflight
	// on every capacity-gate Acquire / Release, SetCapacity once at
	// startup. nil disables emission entirely.
	Recorder Recorder

	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
}

// Default tuning values used when Config zero-values them. Aligned
// with the CP signed-write endpoints (60s skew window, 5min nonce
// TTL) so peers experiencing clock drift see uniform behaviour.
const (
	DefaultSkewWindow = 60 * time.Second
	DefaultNonceTTL   = 5 * time.Minute
	// DefaultMaxBodyBytes caps inbound peer-overlay request bodies
	// at 4 MiB. Anthropic Messages requests rarely exceed a few
	// hundred KB; streamed responses are not bounded by this limit.
	DefaultMaxBodyBytes = 4 << 20
)

// NewServerWithConfig constructs a full Server. When cfg.GatewayHandler
// is nil the result is equivalent to NewServer(cfg.DeviceName).
func NewServerWithConfig(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	skew := cfg.SkewWindow
	if skew <= 0 {
		skew = DefaultSkewWindow
	}
	ttl := cfg.NonceTTL
	if ttl <= 0 {
		ttl = DefaultNonceTTL
	}
	maxBody := cfg.MaxBodySize
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	s := &Server{
		deviceName:      cfg.DeviceName,
		gw:              cfg.GatewayHandler,
		peerLookup:      cfg.PeerLookup,
		nonces:          cfg.NonceCache,
		skewWindow:      skew,
		nonceTTL:        ttl,
		maxBodySize:     maxBody,
		now:             now,
		pausedGate:      pausedGateAdapter(cfg.IsPaused),
		inferenceGate:   inferenceGateAdapter(cfg.IsInferenceDisabled),
		shareGate:       shareGateAdapter(cfg.IsShareDenied),
		isPausedFn:      cfg.IsPaused,
		isShareDeniedFn: cfg.IsShareDenied,
		engineReadyFn:   cfg.EngineReadyFn,
		recorder:        cfg.Recorder,
	}
	// The counter + gate are always wired (even at Capacity 0 = unlimited)
	// so the control plane can retune the cap live via SetCapacity once the
	// device's effective capacity arrives on the network map. At cap 0 the
	// gate short-circuits in Acquire, so the only cost is one extra middleware
	// hop on the hot path — cheap, and the price of admin-tunable admission.
	s.inflight = newInflightCounter(cfg.Capacity)
	s.capacityGate = capacityGateAdapter(s.inflight, cfg.Recorder)
	if cfg.Recorder != nil {
		cfg.Recorder.SetCapacity(cfg.Capacity)
	}
	return s
}

// SetCapacity retunes the overlay listener's concurrent-request admission
// ceiling live (n <= 0 ⇒ unlimited). Driven from the network-map stream: the
// CP folds the admin per-device max-clients override into the served
// nm.Self.InferenceState.Capacity, and the agent applies it here so the
// serving side enforces the same cap the requesting peers observe. No-op on a
// ping-only server (NewServer, no inflight counter).
func (s *Server) SetCapacity(n int) {
	if s.inflight == nil {
		return
	}
	s.inflight.setCapacity(n)
	if s.recorder != nil {
		s.recorder.SetCapacity(n)
	}
}

// Handler returns the http.Handler for the overlay listener. It is
// split out from ServeOverlay so unit tests can drive it via httptest
// without spinning up netstack.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/ping", s.handlePing)
	if s.gw != nil {
		guarded := s.peerAuthChain(s.gw.Handler())
		// Both the OpenAI and Anthropic surfaces share the peer-auth
		// chain. The gateway.HandlerSet route table covers /v1/models,
		// /v1/chat/completions, /v1/responses, /anthropic/v1/messages,
		// /anthropic/v1/messages/count_tokens.
		mux.Handle("/v1/", guarded)
		mux.Handle("/anthropic/", guarded)

		// Phase 8: /waired/v1/inference/healthz reports current gate
		// + capacity + engine state. Auth is required (wgPeerOnly +
		// verifyPeerSignature) so leaking the state to anyone able to
		// hit the overlay listener is impossible, but the gates
		// themselves are deliberately bypassed — the body conveys the
		// gate state to the remote probe client.
		healthz := s.peerAuthOnly(http.HandlerFunc(s.handleHealthz))
		mux.Handle("/waired/v1/inference/healthz", healthz)
	}
	return mux
}

// peerAuthChain composes the middleware stack the overlay inference
// routes ride behind. Order (outermost → innermost):
//
//	wgPeerOnly         (= source IP must resolve to a known peer)
//	verifyPeerSignature (= Ed25519 over canonical headers + body)
//	pausedGate         (= 503 waired_paused while paused)
//	inferenceGate      (= 503 waired_inference_disabled while disabled)
//	shareGate          (= 503 waired_inference_not_shared while mesh-share opted out)
//	capacityGate       (= 503 waired_inference_overloaded above Config.Capacity)
//
// capacityGate sits innermost so the operator's existence/visibility
// gates fire first (a paused/disabled/un-shared agent should NOT burn
// an admission slot just to return 503). Within the visibility gates,
// shareGate sits just outside capacityGate so an un-shared agent's
// rejection reads as a privacy decision rather than overload — a
// rotating set of pre-existing 503 envelopes still gets the right
// type even when both conditions would apply.
//
// Pausing the agent, disabling inference, unsharing, or saturating
// mid-flight rejects peer requests with the same JSON envelope a
// loopback request would see — callers thus get a uniform error
// contract regardless of which side of the mesh originated the
// request.
func (s *Server) peerAuthChain(next http.Handler) http.Handler {
	if s.capacityGate != nil {
		next = s.capacityGate(next)
	}
	if s.shareGate != nil {
		next = s.shareGate(next)
	}
	if s.inferenceGate != nil {
		next = s.inferenceGate(next)
	}
	if s.pausedGate != nil {
		next = s.pausedGate(next)
	}
	next = verifyPeerSignature(next, s.peerLookup, s.nonces, s.skewWindow, s.nonceTTL, s.maxBodySize, s.now)
	next = wgPeerOnly(next, s.peerLookup)
	return next
}

// peerAuthOnly wraps next in just the authentication layers
// (wgPeerOnly + verifyPeerSignature) without the operator gates. Used
// for /waired/v1/inference/healthz: probes from authenticated peers
// must always receive the current state in the JSON body, even when
// the agent is paused / inference-disabled / share-denied / at
// capacity — the gates would mask that information behind a single
// 503, defeating the probe's purpose. The body keeps the state, the
// gates still apply to the actual inference path.
func (s *Server) peerAuthOnly(next http.Handler) http.Handler {
	next = verifyPeerSignature(next, s.peerLookup, s.nonces, s.skewWindow, s.nonceTTL, s.maxBodySize, s.now)
	next = wgPeerOnly(next, s.peerLookup)
	return next
}

// ServeOverlay accepts connections on the supplied overlay listener
// until the listener is closed.
func (s *Server) ServeOverlay(ctx context.Context, l net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PingResponse{
		OK:     true,
		Device: s.deviceName,
		Time:   nowOrTime(s.now).UTC().Format(time.RFC3339Nano),
	})
}

func nowOrTime(fn func() time.Time) time.Time {
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// pausedGateAdapter wraps an isPaused closure into the standard
// http.Handler-decorator shape. Returns nil when fn is nil so the
// chain skips it.
func pausedGateAdapter(fn func() bool) func(http.Handler) http.Handler {
	if fn == nil {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fn() {
				writeOverlay503(w, "waired_paused",
					"waired-agent is paused; peer-engine routing is disabled until `waired resume`.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func inferenceGateAdapter(fn func() bool) func(http.Handler) http.Handler {
	if fn == nil {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fn() {
				writeOverlay503(w, "waired_inference_disabled",
					"waired-agent inference engine is disabled on this peer.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// shareGateAdapter rejects peer requests when this agent has opted out
// of mesh-share (Phase 6). Sits innermost so the operator's privacy
// choice surfaces as a typed error envelope rather than blending into
// the broader "engine disabled" reply.
func shareGateAdapter(fn func() bool) func(http.Handler) http.Handler {
	if fn == nil {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fn() {
				writeOverlay503(w, "waired_inference_not_shared",
					"waired-agent on this peer is not currently sharing its local inference engine with the mesh.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// capacityGateAdapter rejects peer requests when the agent is already
// servicing Config.Capacity concurrent requests. Defers Release on
// the way back out via the standard middleware pattern; a panic
// inside the downstream handler is still tracked correctly because
// the Release is deferred before next.ServeHTTP runs.
//
// Returns nil when counter is nil so the chain skips the wrapper.
//
// When rec is non-nil it receives a SetInflight gauge update on every
// Acquire / Release plus a RecordServed call at request termination
// (status captured via a thin ResponseWriter wrapper so 2xx and 5xx
// surfaces are distinguished without intercepting the body).
func capacityGateAdapter(counter *inflightCounter, rec Recorder) func(http.Handler) http.Handler {
	if counter == nil {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !counter.Acquire() {
				writeOverlay503(w, "waired_inference_overloaded",
					"waired-agent on this peer is at its concurrent-request capacity; retry on another peer or wait.")
				return
			}
			if rec != nil {
				rec.SetInflight(int(counter.InFlight()))
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			defer func() {
				counter.Release()
				if rec != nil {
					rec.SetInflight(int(counter.InFlight()))
					result := "success"
					if sw.status >= 400 {
						result = "error"
					}
					rec.RecordServed(result, uint32(time.Since(start).Milliseconds()))
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// statusWriter captures the first WriteHeader status code so the
// capacity-gate adapter can classify the served result for telemetry.
// Defaults to 200 when the handler never explicitly writes a header
// (http.ResponseWriter's documented behaviour).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush propagates SSE flushing through the wrapper. Inference
// requests stream tokens; without this declaration the wrapper would
// hide http.Flusher implementations from upstream.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// writeOverlay503 writes the same JSON shape the loopback gateway
// uses, so a peer that proxies a 503 response back to its client sees
// a uniform error envelope.
func writeOverlay503(w http.ResponseWriter, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}
