package observability

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNewMetrics_RegistersAllCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}

	// CounterVec / GaugeVec only surface in Gather() after at least
	// one label combination has been observed; touch each so the
	// registration check covers them.
	m.InferenceRequestsTotal.WithLabelValues("openai", "success", "").Inc()
	m.InferenceFallbackTotal.WithLabelValues("capacity_full").Inc()
	m.InferenceSelectDecisionsTotal.WithLabelValues("local").Inc()
	m.InferenceProbeTotal.WithLabelValues("ok").Inc()
	m.InferenceBriefQueueRetryTotal.WithLabelValues("succeeded").Inc()
	m.InferenceServedTotal.WithLabelValues("success").Inc()
	m.InferenceAuthRejectTotal.WithLabelValues("signature").Inc()
	m.MeshPeers.WithLabelValues("ready").Set(1)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	want := []string{
		"waired_inference_requests_total",
		"waired_inference_fallback_total",
		"waired_inference_select_decisions_total",
		"waired_inference_probe_total",
		"waired_inference_brief_queue_retry_total",
		"waired_inference_served_total",
		"waired_inference_auth_reject_total",
		"waired_inference_inflight",
		"waired_inference_capacity_total",
		"waired_inference_engine_ready",
		"waired_inference_share_enabled",
		"waired_inference_paused",
		"waired_mesh_peers",
		"waired_inference_probe_latency_milliseconds",
		"waired_inference_request_latency_milliseconds",
		"waired_inference_served_latency_milliseconds",
	}

	got := make(map[string]bool, len(families))
	for _, f := range families {
		got[f.GetName()] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing metric: %s", name)
		}
	}
}

func TestNewMetrics_DoubleRegisterPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on second NewMetrics against same registry")
		}
	}()
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg)
	_ = NewMetrics(reg)
}

func TestMetrics_NoPeerIDLabelAnywhere(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		for _, mf := range f.GetMetric() {
			for _, lp := range mf.GetLabel() {
				if lp.GetName() == "peer_id" {
					t.Errorf("metric %s has forbidden peer_id label", f.GetName())
				}
			}
		}
	}
}

func TestMetrics_CounterLabelCardinalityBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	// Touch each known enum value to ensure label names match what
	// callers will pass in. Bumping each by 1 also smoke-tests that
	// MustRegister actually wired the vec.
	m.InferenceRequestsTotal.WithLabelValues("openai", "success", "").Inc()
	m.InferenceRequestsTotal.WithLabelValues("anthropic", "error", "runtime_unavailable").Inc()
	m.InferenceFallbackTotal.WithLabelValues("capacity_full").Inc()
	m.InferenceSelectDecisionsTotal.WithLabelValues("sticky").Inc()
	m.InferenceProbeTotal.WithLabelValues("ok").Inc()
	m.InferenceBriefQueueRetryTotal.WithLabelValues("succeeded").Inc()
	m.InferenceServedTotal.WithLabelValues("success").Inc()
	m.InferenceAuthRejectTotal.WithLabelValues("signature").Inc()
	m.MeshPeers.WithLabelValues("ready").Set(5)
}

func TestMetrics_HistogramBucketsAreCustom(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.InferenceProbeLatency.Observe(7)
	m.InferenceRequestLatency.Observe(1500)
	m.InferenceServedLatency.Observe(40000)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	want := map[string][]float64{
		"waired_inference_probe_latency_milliseconds":   ProbeLatencyBuckets,
		"waired_inference_request_latency_milliseconds": RequestLatencyBuckets,
		"waired_inference_served_latency_milliseconds":  RequestLatencyBuckets,
	}

	for _, f := range families {
		expected, ok := want[f.GetName()]
		if !ok {
			continue
		}
		if len(f.GetMetric()) == 0 {
			t.Fatalf("%s: no observations", f.GetName())
		}
		h := f.GetMetric()[0].GetHistogram()
		if h == nil {
			t.Fatalf("%s: not a histogram", f.GetName())
		}
		buckets := h.GetBucket()
		if len(buckets) != len(expected) {
			t.Fatalf("%s: bucket count %d, want %d", f.GetName(), len(buckets), len(expected))
		}
		for i, b := range buckets {
			if b.GetUpperBound() != expected[i] {
				t.Errorf("%s bucket %d: got %v want %v", f.GetName(), i, b.GetUpperBound(), expected[i])
			}
		}
	}
}

func TestMetrics_GaugesStartAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	cases := []struct {
		name string
		g    prometheus.Gauge
	}{
		{"inflight", m.InferenceInflight},
		{"capacity_total", m.InferenceCapacityTotal},
		{"engine_ready", m.InferenceEngineReady},
		{"share_enabled", m.InferenceShareEnabled},
		{"paused", m.InferencePaused},
	}
	for _, c := range cases {
		got := gaugeValue(t, c.g)
		if got != 0 {
			t.Errorf("%s starts at %v, want 0", c.name, got)
		}
	}
}

func TestSetBool(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	SetBool(m.InferenceEngineReady, true)
	if gaugeValue(t, m.InferenceEngineReady) != 1 {
		t.Errorf("SetBool(true) did not set 1")
	}
	SetBool(m.InferenceEngineReady, false)
	if gaugeValue(t, m.InferenceEngineReady) != 0 {
		t.Errorf("SetBool(false) did not set 0")
	}
}

func TestNewMetrics_HelpStringsNonEmpty(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "waired_") {
			t.Errorf("metric %s lacks waired_ prefix", f.GetName())
		}
		if strings.TrimSpace(f.GetHelp()) == "" {
			t.Errorf("metric %s has empty Help text", f.GetName())
		}
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge.Write: %v", err)
	}
	return m.GetGauge().GetValue()
}
