package inference

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signedreq"
)

// blockingGateway is a gatewayHandlerSet whose Handler() blocks on a
// per-request channel. Tests use it to keep N requests "in flight"
// while sending an N+1th to assert capacityGate rejects.
type blockingGateway struct {
	mu       sync.Mutex
	gates    []chan struct{} // one channel per in-flight call
	pending  int             // number of handlers currently parked
	released atomic.Int32    // how many handlers have been released
}

func newBlockingGateway() *blockingGateway { return &blockingGateway{} }

func (b *blockingGateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		gate := make(chan struct{})
		b.gates = append(b.gates, gate)
		b.pending++
		b.mu.Unlock()
		<-gate
		b.released.Add(1)
		w.WriteHeader(http.StatusOK)
	})
}

// release closes one parked gate (FIFO). Returns false if none parked.
func (b *blockingGateway) release() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.gates) == 0 {
		return false
	}
	close(b.gates[0])
	b.gates = b.gates[1:]
	b.pending--
	return true
}

func (b *blockingGateway) inFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

// waitForInFlight spins until at least n handlers are parked or the
// deadline expires. Test scaffolding for the multi-goroutine cases.
func (b *blockingGateway) waitForInFlight(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.inFlight() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waited 2s for %d in-flight handlers, only saw %d", n, b.inFlight())
}

// signedReq builds a freshly-signed peer request with a unique nonce —
// callers can use it to exercise admission without colliding with the
// nonce cache.
func signedReq(t *testing.T, target string, body []byte, deviceID string, priv ed25519.PrivateKey, now time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(string(body)))
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	signedreq.SetHeaderEnvelope(r.Header, priv, deviceID, now.UTC().Format(time.RFC3339), base64.StdEncoding.EncodeToString(nonce), body)
	r.RemoteAddr = peerOverlayIP + ":54321"
	return r
}

// TestOverlayServer_CapacityZeroIsUnlimited confirms that when Config.Capacity
// is unset (= 0), the capacity gate admits unbounded requests. The gate is
// still wired (so the CP can retune the cap live via SetCapacity), but at
// cap 0 Acquire short-circuits and never rejects — the backward-compat path
// pre-Phase-7 agents and external openai-compat endpoints rely on.
func TestOverlayServer_CapacityZeroIsUnlimited(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newFakeGateway()
	s, _, _ := newOverlayServer(t, gw, peer)
	if s.capacityGate == nil {
		t.Fatalf("capacityGate should be wired even at Config.Capacity == 0 (live-tunable)")
	}

	// Run 5 sequential requests; all must succeed regardless of
	// (non-existent) capacity tracking.
	for i := 0; i < 5; i++ {
		req := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}

// TestOverlayServer_CapacityRejectsAboveLimit drives N+1 concurrent
// requests through a server with Capacity=N; exactly N must hit the
// downstream handler and the extra one(s) must get 503
// waired_inference_overloaded with the standard JSON envelope.
func TestOverlayServer_CapacityRejectsAboveLimit(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newBlockingGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.Capacity = 2
	})
	if s.capacityGate == nil {
		t.Fatalf("capacityGate should be wired when Config.Capacity > 0")
	}

	// Send 2 requests that will park in the blocking gateway.
	for i := 0; i < 2; i++ {
		req := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
		go func() {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
		}()
	}
	gw.waitForInFlight(t, 2)

	// Third request arrives while the gateway is at capacity.
	req3 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req3)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("3rd req: status=%d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "waired_inference_overloaded") {
		t.Errorf("3rd req body=%s, want waired_inference_overloaded", rec.Body.String())
	}
	if got := gw.inFlight(); got != 2 {
		t.Errorf("downstream in-flight after 503 = %d, want still 2 (rejected req must not enter handler)", got)
	}

	// Drain the in-flight requests so the test goroutines exit.
	for gw.release() {
	}
}

// TestOverlayServer_CapacityReleasesAfterCompletion ensures
// `defer Release()` runs cleanly: after a request completes, the
// freed slot must admit the next one. A missed Release would brick
// the agent at exactly the operator-set capacity.
func TestOverlayServer_CapacityReleasesAfterCompletion(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newBlockingGateway()
	s, _, _ := newOverlayServer(t, gw, peer, func(c *Config) {
		c.Capacity = 1
	})

	// First req parks.
	req1 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() { defer close(done1); s.Handler().ServeHTTP(rec1, req1) }()
	gw.waitForInFlight(t, 1)

	// Second req while saturated → 503.
	req2 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("2nd req while at cap: status=%d, want 503", rec2.Code)
	}

	// Release the first req so it completes and the slot frees.
	if !gw.release() {
		t.Fatal("no parked gate to release")
	}
	<-done1
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st req final status=%d, want 200", rec1.Code)
	}

	// Third req should now be admitted (slot freed by Release).
	req3 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec3 := httptest.NewRecorder()
	done3 := make(chan struct{})
	go func() { defer close(done3); s.Handler().ServeHTTP(rec3, req3) }()
	gw.waitForInFlight(t, 1)
	if !gw.release() {
		t.Fatal("3rd req did not park; capacity was not properly released")
	}
	<-done3
	if rec3.Code != http.StatusOK {
		t.Fatalf("3rd req: status=%d, want 200 after slot released", rec3.Code)
	}
}

// TestInFlightCounter_AcquireReleaseRaceFree exercises the atomic
// counter directly under heavy concurrency. Run under `go test -race`
// to catch any inadvertent ordinary read/write of n.
func TestInFlightCounter_AcquireReleaseRaceFree(t *testing.T) {
	c := newInflightCounter(8)
	const workers = 32
	const iter = 1000
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iter; j++ {
				if c.Acquire() {
					// At any point under cap, in-flight must not
					// exceed the cap; sample 1% of the time to
					// keep the read pressure low.
					if j%100 == 0 {
						if cap := int(c.capacity.Load()); int(c.InFlight()) > cap {
							t.Errorf("InFlight %d > cap %d during race", c.InFlight(), cap)
						}
					}
					c.Release()
				}
			}
		}()
	}
	wg.Wait()
	if got := c.InFlight(); got != 0 {
		t.Errorf("after balanced Acquire/Release in-flight = %d, want 0", got)
	}
}

// TestInFlightCounter_Capacity0AlwaysAccepts confirms the documented
// "0 = unlimited" semantics: every Acquire returns true and the count
// still tracks for future metrics.
func TestInFlightCounter_Capacity0AlwaysAccepts(t *testing.T) {
	c := newInflightCounter(0)
	for i := 0; i < 100; i++ {
		if !c.Acquire() {
			t.Fatalf("Acquire #%d returned false with capacity=0", i)
		}
	}
	if got := c.InFlight(); got != 100 {
		t.Errorf("InFlight=%d after 100 Acquires with cap=0, want 100", got)
	}
	for i := 0; i < 100; i++ {
		c.Release()
	}
	if got := c.InFlight(); got != 0 {
		t.Errorf("InFlight=%d after 100 Releases, want 0", got)
	}
}

// TestInFlightCounter_SetCapacityLive confirms the admission ceiling can be
// retuned live: tightening rejects new requests immediately (while existing
// in-flight requests drain naturally), and relaxing back to 0 restores
// unlimited admission.
func TestInFlightCounter_SetCapacityLive(t *testing.T) {
	c := newInflightCounter(0) // start unlimited
	for i := 0; i < 3; i++ {
		if !c.Acquire() {
			t.Fatalf("unlimited Acquire #%d returned false", i)
		}
	}

	// Tighten to 2 while 3 are in flight: new Acquire rejects (no forced
	// decrement of the over-cap in-flight requests).
	c.setCapacity(2)
	if c.Acquire() {
		t.Fatalf("Acquire should reject: in-flight 3 >= new cap 2")
	}

	// Drain below the cap; the next Acquire is admitted.
	c.Release()
	c.Release() // in-flight = 1
	if !c.Acquire() {
		t.Fatalf("Acquire should succeed once in-flight (1) < cap (2)")
	}

	// Relax to unlimited: always accepts again.
	c.setCapacity(0)
	for i := 0; i < 5; i++ {
		if !c.Acquire() {
			t.Fatalf("relaxed Acquire #%d returned false", i)
		}
	}
}

// TestOverlayServer_SetCapacityLive exercises the CP-driven retune end to end
// through the middleware chain: a server constructed unlimited (Capacity 0)
// starts rejecting once SetCapacity tightens the cap, then admits again when
// it is relaxed back to 0 — the path the network-map stream drives from
// nm.Self.InferenceState.Capacity.
func TestOverlayServer_SetCapacityLive(t *testing.T) {
	pub, priv := mustKey(t)
	peer := PeerIdentity{DeviceID: "peer-A", MachineKey: pub}
	gw := newBlockingGateway()
	s, _, _ := newOverlayServer(t, gw, peer) // Capacity 0 = unlimited

	// Admin sets max_clients = 1; the CP folds it into Self.Capacity and the
	// agent applies it here.
	s.SetCapacity(1)

	req1 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() { defer close(done1); s.Handler().ServeHTTP(rec1, req1) }()
	gw.waitForInFlight(t, 1)

	// Second concurrent request now rejected by the live cap.
	req2 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("2nd req under live cap=1: status=%d want 503", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "waired_inference_overloaded") {
		t.Errorf("2nd req body=%s, want waired_inference_overloaded", rec2.Body.String())
	}

	// Relax back to unlimited; a 2nd concurrent request is admitted again.
	s.SetCapacity(0)
	req3 := signedReq(t, "/v1/chat/completions", []byte(`{}`), "peer-A", priv, time.Date(2026, 5, 9, 18, 0, 0, 0, time.UTC))
	rec3 := httptest.NewRecorder()
	done3 := make(chan struct{})
	go func() { defer close(done3); s.Handler().ServeHTTP(rec3, req3) }()
	gw.waitForInFlight(t, 2)

	// Drain everything so the test goroutines exit.
	for gw.release() {
	}
	<-done1
	<-done3
}
