package inference

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// captureRecorder records every emit for assertion.
type captureRecorder struct {
	mu       sync.Mutex
	served   []servedCall
	inflight []int
	capacity []int
}

type servedCall struct {
	result    string
	latencyMs uint32
}

func (c *captureRecorder) RecordServed(result string, latencyMs uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.served = append(c.served, servedCall{result, latencyMs})
}

func (c *captureRecorder) SetInflight(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight = append(c.inflight, n)
}

func (c *captureRecorder) SetCapacity(total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacity = append(c.capacity, total)
}

func (c *captureRecorder) snapServed() []servedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]servedCall, len(c.served))
	copy(out, c.served)
	return out
}

func (c *captureRecorder) snapInflight() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, len(c.inflight))
	copy(out, c.inflight)
	return out
}

func TestNewServerWithConfig_EmitsSetCapacity(t *testing.T) {
	rec := &captureRecorder{}
	_ = NewServerWithConfig(Config{
		DeviceName: "test",
		Capacity:   10,
		Recorder:   rec,
	})
	rec.mu.Lock()
	caps := append([]int{}, rec.capacity...)
	rec.mu.Unlock()
	if len(caps) != 1 || caps[0] != 10 {
		t.Fatalf("SetCapacity calls: %v, want [10]", caps)
	}
}

func TestNewServerWithConfig_NoRecorderIsSafe(t *testing.T) {
	_ = NewServerWithConfig(Config{
		DeviceName: "test",
		Capacity:   5,
	})
	// No assertion — just confirm it doesn't panic.
}

func TestCapacityGateAdapter_EmitsServedAndInflight_Success(t *testing.T) {
	rec := &captureRecorder{}
	counter := newInflightCounter(2)
	gate := capacityGateAdapter(counter, rec, nil, nil)

	called := atomic.Bool{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	h := gate(inner)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)

	if !called.Load() {
		t.Fatal("inner handler not invoked")
	}
	served := rec.snapServed()
	if len(served) != 1 {
		t.Fatalf("served calls: %d, want 1", len(served))
	}
	if served[0].result != "success" {
		t.Errorf("result: %q want success", served[0].result)
	}
	inflight := rec.snapInflight()
	if len(inflight) != 2 {
		t.Fatalf("inflight calls: %d, want 2 (acquire+release)", len(inflight))
	}
	if inflight[0] != 1 || inflight[1] != 0 {
		t.Errorf("inflight trace: got %v want [1 0]", inflight)
	}
}

func TestCapacityGateAdapter_EmitsServedError(t *testing.T) {
	rec := &captureRecorder{}
	counter := newInflightCounter(2)
	gate := capacityGateAdapter(counter, rec, nil, nil)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	h := gate(inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	served := rec.snapServed()
	if len(served) != 1 || served[0].result != "error" {
		t.Fatalf("served result: got %+v want error", served)
	}
}

func TestCapacityGateAdapter_OverloadDoesNotEmitServed(t *testing.T) {
	rec := &captureRecorder{}
	counter := newInflightCounter(1)
	// Pre-acquire the only slot so the next request is overloaded.
	if !counter.Acquire() {
		t.Fatal("setup acquire failed")
	}
	gate := capacityGateAdapter(counter, rec, nil, nil)

	called := atomic.Bool{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	})
	h := gate(inner)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if called.Load() {
		t.Fatal("inner handler should not run when capacity full")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", w.Code)
	}
	if served := rec.snapServed(); len(served) != 0 {
		t.Errorf("overload should not emit RecordServed; got %+v", served)
	}
	if inflight := rec.snapInflight(); len(inflight) != 0 {
		t.Errorf("overload should not emit SetInflight; got %v", inflight)
	}
}

func TestCapacityGateAdapter_NilRecorderIsSafe(t *testing.T) {
	counter := newInflightCounter(1)
	gate := capacityGateAdapter(counter, nil, nil, nil)
	if gate == nil {
		t.Fatal("gate should be non-nil even with nil recorder")
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := gate(inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestCapacityGateAdapter_NilCounterReturnsNil(t *testing.T) {
	rec := &captureRecorder{}
	if got := capacityGateAdapter(nil, rec, nil, nil); got != nil {
		t.Errorf("nil counter should return nil gate; got %T", got)
	}
}
