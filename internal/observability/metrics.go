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

// TTFTBuckets covers time-to-first-token, which is NOT distributed like
// end-to-end latency and cannot share RequestLatencyBuckets. TTFT is
// bimodal with the modes roughly 30x apart, and the metric's whole job
// is telling them apart on a given host: a warm prefix-cache hit
// (measured 0.21-4.09 s across the reference hosts) against a cold full
// prefill of the same prompt (9.4-45.3 s). A uniform ~2x ladder puts
// those two corpora in disjoint bands, where RequestLatencyBuckets'
// deliberately coarse tail (10000 -> 30000 -> 60000) collapses them.
//
// Measurements: docs/knowledges/20260819/2130-local-engine-caches-the-prefix.md
// and docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md.
var TTFTBuckets = []float64{100, 200, 400, 800, 1500, 3000, 6000, 12000, 25000, 50000}

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

	// Token counters (waired#829). Partitioned by API kind only —
	// deliberately not by peer or model, keeping the series count
	// bounded per the note above.
	InferenceInputTokensTotal  *prometheus.CounterVec
	InferenceOutputTokensTotal *prometheus.CounterVec

	// InferenceCachedInputTokensTotal is the subset of
	// InferenceInputTokensTotal the engine served from its prefix cache
	// (waired-agent#885); their ratio is the hit rate. Same {kind} label
	// as the pair above so the division is well-formed.
	InferenceCachedInputTokensTotal *prometheus.CounterVec

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

	// InferenceTTFT is observed only where the first-token instant is
	// visible (the Anthropic streaming leg), and only when it was actually
	// observed — see Recorder.RecordRequest. Unlabelled, like its
	// neighbours: a "kind" label would carry exactly one value today and
	// would imply the other kinds report zero, which is the opposite of
	// what an absent observation means.
	InferenceTTFT          prometheus.Histogram
	InferenceServedLatency prometheus.Histogram
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

		InferenceInputTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_input_tokens_total",
			Help: "Prompt tokens reported by the engine for requests served through this agent's gateway, partitioned by API kind.",
		}, []string{"kind"}),

		InferenceOutputTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_output_tokens_total",
			Help: "Completion tokens reported by the engine for requests served through this agent's gateway, partitioned by API kind.",
		}, []string{"kind"}),

		InferenceCachedInputTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waired_inference_cached_input_tokens_total",
			Help: "Prompt tokens the engine served from its prefix cache instead of prefilling, partitioned by API kind. Divide by waired_inference_input_tokens_total for the hit rate. Only engines that report a prompt-token breakdown move this.",
		}, []string{"kind"}),

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

		InferenceTTFT: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "waired_inference_ttft_milliseconds",
			Help:    "Time to first token of streamed inference requests served via this agent's gateway, in milliseconds. Observed only where the first-token instant is visible.",
			Buckets: TTFTBuckets,
		}),
	}

	reg.MustRegister(
		m.InferenceRequestsTotal,
		m.InferenceInputTokensTotal,
		m.InferenceOutputTokensTotal,
		m.InferenceCachedInputTokensTotal,
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
		m.InferenceTTFT,
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
