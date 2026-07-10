package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func newTestRecorder(t *testing.T) (*Recorder, *Ring, *Metrics, *bytes.Buffer) {
	t.Helper()
	ring := NewRing(128)
	metrics := NewMetrics(prometheus.NewRegistry())
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return NewRecorder(ring, metrics, logger), ring, metrics, buf
}

func TestRecorder_NilSafe(t *testing.T) {
	var rec *Recorder
	rec.RecordRequest(RequestEvent{})
	rec.RecordFallback(FallbackEvent{})
	rec.RecordBriefQueueRetry("succeeded")
	rec.RecordProbe("ok", 5)
	rec.RecordSelection("local", "", "m")
	rec.RecordServed("success", 100)
	rec.RecordAuthReject("signature")
	rec.SetEngineReady(true, "")
	rec.SetShareEnabled(true, "")
	rec.SetPaused(false, "")
	rec.SetCapacity(10)
	rec.SetInflight(3)
	rec.SetMeshPeers(1, 1, 1)
	if rec.Ring() != nil {
		t.Fatal("nil receiver Ring() should return nil")
	}
}

func TestRecorder_RecordRequest_FansOutToAllSinks(t *testing.T) {
	rec, ring, metrics, buf := newTestRecorder(t)
	rec.RecordRequest(RequestEvent{
		Kind: "openai", Model: "qwen3:8b", Decision: "remote",
		PeerID: "peer-c", Status: 200, LatencyMs: 312,
	})

	// Sink 1: ring
	events, _, _ := ring.Since(0, nil, 0)
	if len(events) != 1 || events[0].Kind != KindRequest {
		t.Fatalf("ring: events=%d kind=%v", len(events), events)
	}
	if events[0].Request.PeerID != "peer-c" {
		t.Errorf("ring request peer: got %q want peer-c", events[0].Request.PeerID)
	}

	// Sink 2: Prom counter & histogram
	if got := counterValue(t, metrics.InferenceRequestsTotal, "openai", "success", ""); got != 1 {
		t.Errorf("requests_total{openai,success}: got %v want 1", got)
	}

	// Sink 3: slog (success path → no warn log)
	if strings.Contains(buf.String(), "inference request error") {
		t.Errorf("success request emitted error-level slog: %s", buf.String())
	}
}

func TestRecorder_RecordRequest_ErrorLogsSlog(t *testing.T) {
	rec, _, _, buf := newTestRecorder(t)
	rec.RecordRequest(RequestEvent{
		Kind: "openai", Status: 503, ErrorReason: "all_overloaded",
	})
	if !strings.Contains(buf.String(), "inference request error") {
		t.Errorf("error request did not log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "all_overloaded") {
		t.Errorf("slog did not include error_reason: %s", buf.String())
	}
}

func TestRecorder_RecordFallback_FansOut(t *testing.T) {
	rec, ring, metrics, buf := newTestRecorder(t)
	rec.RecordFallback(FallbackEvent{From: "peer-a", To: "peer-c", Reason: "capacity_full", Model: "m"})

	events, _, _ := ring.Since(0, nil, 0)
	if len(events) != 1 || events[0].Kind != KindFallback {
		t.Fatalf("ring: %+v", events)
	}
	if got := counterValue(t, metrics.InferenceFallbackTotal, "capacity_full"); got != 1 {
		t.Errorf("fallback_total{capacity_full}: got %v want 1", got)
	}
	if !strings.Contains(buf.String(), "inference fallback") {
		t.Errorf("slog did not record fallback: %s", buf.String())
	}
}

func TestRecorder_RecordProbe_MetricsOnly(t *testing.T) {
	rec, ring, metrics, _ := newTestRecorder(t)
	rec.RecordProbe("ok", 7)
	rec.RecordProbe("auth_error", 12)

	events, _, _ := ring.Since(0, nil, 0)
	if len(events) != 0 {
		t.Errorf("probe should not go to ring; got %d events", len(events))
	}
	if got := counterValue(t, metrics.InferenceProbeTotal, "ok"); got != 1 {
		t.Errorf("probe_total{ok}: got %v want 1", got)
	}
	if got := counterValue(t, metrics.InferenceProbeTotal, "auth_error"); got != 1 {
		t.Errorf("probe_total{auth_error}: got %v want 1", got)
	}
}

func TestRecorder_RecordSelection_FansOutToRingAndCounter(t *testing.T) {
	rec, ring, metrics, _ := newTestRecorder(t)
	rec.RecordSelection("remote", "peer-c", "m")

	events, _, _ := ring.Since(0, nil, 0)
	if len(events) != 1 || events[0].Kind != KindSelection {
		t.Fatalf("ring: %+v", events)
	}
	if events[0].Selection.PeerID != "peer-c" {
		t.Errorf("ring selection peer: got %q want peer-c", events[0].Selection.PeerID)
	}
	if got := counterValue(t, metrics.InferenceSelectDecisionsTotal, "remote"); got != 1 {
		t.Errorf("select_decisions_total{remote}: got %v want 1", got)
	}
}

func TestRecorder_SetEngineReady_EmitsEventOnTransition(t *testing.T) {
	rec, ring, metrics, buf := newTestRecorder(t)
	// First Set establishes baseline; no event.
	rec.SetEngineReady(true, "boot")
	events, _, _ := ring.Since(0, nil, 0)
	if len(events) != 0 {
		t.Fatalf("first SetEngineReady should not emit event; got %d", len(events))
	}
	if g := gaugeValue(t, metrics.InferenceEngineReady); g != 1 {
		t.Errorf("engine_ready gauge: got %v want 1", g)
	}

	// Same value → no event.
	rec.SetEngineReady(true, "noop")
	events, _, _ = ring.Since(0, nil, 0)
	if len(events) != 0 {
		t.Fatalf("same value should not emit; got %d", len(events))
	}

	// Transition → event.
	rec.SetEngineReady(false, "ollama_died")
	events, _, _ = ring.Since(0, nil, 0)
	if len(events) != 1 || events[0].Kind != KindEngineStateChange {
		t.Fatalf("transition: %+v", events)
	}
	esc := events[0].EngineStateChange
	if esc.From != "ready" || esc.To != "not_ready" {
		t.Errorf("transition tags: %+v", esc)
	}
	if esc.Reason != "ollama_died" {
		t.Errorf("transition reason: got %q want ollama_died", esc.Reason)
	}
	if !strings.Contains(buf.String(), "engine state change") {
		t.Errorf("slog did not log state change: %s", buf.String())
	}
}

func TestRecorder_SetShareEnabled_EmitsTransitionEvent(t *testing.T) {
	rec, ring, _, _ := newTestRecorder(t)
	rec.SetShareEnabled(true, "")
	rec.SetShareEnabled(false, "user")

	events, _, _ := ring.Since(0, []Kind{KindEngineStateChange}, 0)
	if len(events) != 1 {
		t.Fatalf("expected 1 transition; got %d", len(events))
	}
	if events[0].EngineStateChange.From != "share_on" || events[0].EngineStateChange.To != "share_off" {
		t.Errorf("share transition: %+v", events[0].EngineStateChange)
	}
}

func TestRecorder_SetPaused_EmitsTransitionEvent(t *testing.T) {
	rec, ring, _, _ := newTestRecorder(t)
	rec.SetPaused(false, "")
	rec.SetPaused(true, "user_request")

	events, _, _ := ring.Since(0, []Kind{KindEngineStateChange}, 0)
	if len(events) != 1 {
		t.Fatalf("expected 1 transition; got %d", len(events))
	}
	if events[0].EngineStateChange.To != "paused" {
		t.Errorf("paused transition to: %q", events[0].EngineStateChange.To)
	}
}

func TestRecorder_SetCapacity_SetInflight_SetMeshPeers(t *testing.T) {
	rec, _, metrics, _ := newTestRecorder(t)
	rec.SetCapacity(10)
	rec.SetInflight(3)
	rec.SetMeshPeers(12, 9, 7)

	if g := gaugeValue(t, metrics.InferenceCapacityTotal); g != 10 {
		t.Errorf("capacity: got %v want 10", g)
	}
	if g := gaugeValue(t, metrics.InferenceInflight); g != 3 {
		t.Errorf("inflight: got %v want 3", g)
	}
	if g := gaugeVecValue(t, metrics.MeshPeers, "enrolled"); g != 12 {
		t.Errorf("mesh enrolled: got %v want 12", g)
	}
	if g := gaugeVecValue(t, metrics.MeshPeers, "reachable"); g != 9 {
		t.Errorf("mesh reachable: got %v want 9", g)
	}
	if g := gaugeVecValue(t, metrics.MeshPeers, "ready"); g != 7 {
		t.Errorf("mesh ready: got %v want 7", g)
	}
}

func TestRecorder_RecordBriefQueueRetry(t *testing.T) {
	rec, _, metrics, _ := newTestRecorder(t)
	rec.RecordBriefQueueRetry("succeeded")
	rec.RecordBriefQueueRetry("succeeded")
	rec.RecordBriefQueueRetry("failed")
	if got := counterValue(t, metrics.InferenceBriefQueueRetryTotal, "succeeded"); got != 2 {
		t.Errorf("succeeded: got %v want 2", got)
	}
	if got := counterValue(t, metrics.InferenceBriefQueueRetryTotal, "failed"); got != 1 {
		t.Errorf("failed: got %v want 1", got)
	}
}

func TestRecorder_RecordServed_RecordAuthReject(t *testing.T) {
	rec, _, metrics, _ := newTestRecorder(t)
	rec.RecordServed("success", 1500)
	rec.RecordServed("error", 200)
	rec.RecordAuthReject("signature")

	if got := counterValue(t, metrics.InferenceServedTotal, "success"); got != 1 {
		t.Errorf("served{success}: %v", got)
	}
	if got := counterValue(t, metrics.InferenceServedTotal, "error"); got != 1 {
		t.Errorf("served{error}: %v", got)
	}
	if got := counterValue(t, metrics.InferenceAuthRejectTotal, "signature"); got != 1 {
		t.Errorf("auth_reject{signature}: %v", got)
	}
}

func TestRecorder_PartialSinks(t *testing.T) {
	// Ring + slog only, metrics nil.
	ring := NewRing(8)
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	rec := NewRecorder(ring, nil, logger)
	rec.RecordRequest(RequestEvent{Kind: "openai", Status: 200, LatencyMs: 100})
	rec.RecordFallback(FallbackEvent{Reason: "x"})

	events, _, _ := ring.Since(0, nil, 0)
	if len(events) != 2 {
		t.Errorf("ring should still get events without metrics: got %d", len(events))
	}
}

func TestRecorder_ConcurrentEmits(t *testing.T) {
	rec, _, _, _ := newTestRecorder(t)
	var wg sync.WaitGroup
	const workers = 8
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec.RecordRequest(RequestEvent{Kind: "openai", Status: 200, LatencyMs: 50})
				rec.RecordProbe("ok", 5)
				rec.SetInflight(j)
			}
		}()
	}
	wg.Wait()
}

// --- helpers ---

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func gaugeVecValue(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	g, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get gauge: %v", err)
	}
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge write: %v", err)
	}
	return m.GetGauge().GetValue()
}
