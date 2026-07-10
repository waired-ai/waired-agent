package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

// ProbeLatencyBuckets is the histogram bucket layout for WG-mesh
// probe latency. The 50 ms budget (Phase 8) sits at bucket 5 so
// p95/p99 readouts span both the in-budget and timed-out tails.
var ProbeLatencyBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500}

// RequestLatencyBuckets covers LLM inference end-to-end: cache hits
// at the low end (50-250 ms), small-model generation in the middle
// (500-5000 ms), and long-context generation at the high end
// (10-60 s). 60000 is the highest bucket; +Inf catches degenerate
// long runs.
var RequestLatencyBuckets = []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}

// Metrics owns every Prometheus collector this agent registers.
// Construction does not call into any prometheus default registry —
// the caller supplies the Registerer so tests can use an isolated
// registry and the agent can choose where to mount.
//
// Cardinality discipline: no peer_id label is exposed anywhere.
// Per-peer drill-down is delegated to the event ring; Prom metrics
// stay at the (per-agent, per-enum) cardinality so a growing mesh
// does not balloon scrape series.
type Metrics struct {
	// --- Tier 1 counters ---

	InferenceRequestsTotal        *prometheus.CounterVec
	InferenceFallbackTotal        *prometheus.CounterVec
	InferenceSelectDecisionsTotal *prometheus.CounterVec

	// --- Tier 2 counters ---

	InferenceProbeTotal                 *prometheus.CounterVec
	InferenceBriefQueueRetryTotal       *prometheus.CounterVec
	InferenceServedTotal                *prometheus.CounterVec
	InferenceAuthRejectTotal            *prometheus.CounterVec
	InferencePinnedPeerUnreachableTotal *prometheus.CounterVec

	// --- Gauges ---

	InferenceInflight      prometheus.Gauge
	InferenceCapacityTotal prometheus.Gauge
	InferenceEngineReady   prometheus.Gauge
	InferenceShareEnabled  prometheus.Gauge
	InferencePaused        prometheus.Gauge
	MeshPeers              *prometheus.GaugeVec

	// --- Histograms ---

	InferenceProbeLatency   prometheus.Histogram
	InferenceRequestLatency prometheus.Histogram
	InferenceServedLatency  prometheus.Histogram
}

// NewMetrics constructs and registers every Phase 9 collector on reg.
// reg == nil registers against prometheus.DefaultRegisterer; tests
// should pass an isolated prometheus.NewRegistry() to avoid global
// state.
//
// Registration is performed via MustRegister; duplicate construction
// against the same registry will panic. Construct exactly once per
// process.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	m := &Metrics{
		InferenceRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_requests_total",
			Help: "Inference requests served by this agent's gateway, partitioned by API kind, terminal result, and error reason.",
		}, []string{"kind", "result", "error_reason"}),

		InferenceFallbackTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_fallback_total",
			Help: "Count of requests whose probe-then-commit winner was not the top-1 candidate, tagged by the top-1 candidate's failure reason.",
		}, []string{"from_reason"}),

		InferenceSelectDecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_select_decisions_total",
			Help: "Selector decisions before probe-then-commit, partitioned by decision class (local / remote / sticky / fallback).",
		}, []string{"decision"}),

		InferenceProbeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_probe_total",
			Help: "Outcomes of /healthz probes against mesh peers, partitioned by outcome enum (ok / legacy_peer / auth_error / transport_error).",
		}, []string{"outcome"}),

		InferenceBriefQueueRetryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_brief_queue_retry_total",
			Help: "Brief-queue (250 ms + 1 retry) outcomes when all probes fail on the first pass.",
		}, []string{"result"}),

		InferenceServedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_served_total",
			Help: "Inference requests this agent served on behalf of mesh peers via the peer-overlay endpoint.",
		}, []string{"result"}),

		InferenceAuthRejectTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_auth_reject_total",
			Help: "Peer-auth chain rejections on the inbound inference endpoint, partitioned by failing check.",
		}, []string{"reason"}),

		InferencePinnedPeerUnreachableTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_pinned_peer_unreachable_total",
			Help: "Manual-routing pin events that did NOT route to the pinned peer, partitioned by reason (unreachable | lacks_model).",
		}, []string{"reason"}),

		InferenceInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "waired_inference_inflight",
			Help: "Current number of in-flight inference requests on this agent.",
		}),

		InferenceCapacityTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "waired_inference_capacity_total",
			Help: "Configured concurrency ceiling (admission capacity) for this agent's inference engine.",
		}),

		InferenceEngineReady: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "waired_inference_engine_ready",
			Help: "1 when this agent's inference engine is ready to accept work, 0 otherwise.",
		}),

		InferenceShareEnabled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "waired_inference_share_enabled",
			Help: "1 when this agent shares its inference engine with the mesh, 0 otherwise.",
		}),

		InferencePaused: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "waired_inference_paused",
			Help: "1 when this agent's inference subsystem is paused, 0 otherwise.",
		}),

		MeshPeers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "waired_mesh_peers",
			Help: "Number of mesh peers in the given lifecycle state (enrolled / reachable / ready).",
		}, []string{"state"}),

		InferenceProbeLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "waired_inference_probe_latency_milliseconds",
			Help:    "Latency of /healthz probes to mesh peers, in milliseconds.",
			Buckets: ProbeLatencyBuckets,
		}),

		InferenceRequestLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "waired_inference_request_latency_milliseconds",
			Help:    "End-to-end latency of inference requests served via this agent's gateway, in milliseconds.",
			Buckets: RequestLatencyBuckets,
		}),

		InferenceServedLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "waired_inference_served_latency_milliseconds",
			Help:    "Latency of inference requests this agent served on behalf of mesh peers, in milliseconds.",
			Buckets: RequestLatencyBuckets,
		}),
	}

	reg.MustRegister(
		m.InferenceRequestsTotal,
		m.InferenceFallbackTotal,
		m.InferenceSelectDecisionsTotal,
		m.InferenceProbeTotal,
		m.InferenceBriefQueueRetryTotal,
		m.InferenceServedTotal,
		m.InferenceAuthRejectTotal,
		m.InferencePinnedPeerUnreachableTotal,
		m.InferenceInflight,
		m.InferenceCapacityTotal,
		m.InferenceEngineReady,
		m.InferenceShareEnabled,
		m.InferencePaused,
		m.MeshPeers,
		m.InferenceProbeLatency,
		m.InferenceRequestLatency,
		m.InferenceServedLatency,
	)

	return m
}

// SetBool is a convenience for gauges that mirror boolean state.
func SetBool(g prometheus.Gauge, v bool) {
	if v {
		g.Set(1)
	} else {
		g.Set(0)
	}
}
