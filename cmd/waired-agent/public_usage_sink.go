package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/inference"
)

// publicUsageSink is the only place the gateway and the peer-auth layer
// meet (waired#829).
//
// internal/gateway must not import internal/inference — it is a generic
// HTTP adapter that knows nothing about peer identity — so the bridge
// lives here in package main, which already imports both. The same
// house style as the ClassifyModel / ContextWindowFor / ResolveUnknownModel
// callbacks.
//
// Returns the gateway.Deps.OnUsage callback, wired ONLY on the :9474
// overlay HandlerSet: the loopback, Claude-intercept and OpenCode
// surfaces serve this device's own operator, whose usage is nobody's to
// report. Their token counts still reach local telemetry — that half is
// unconditional in the gateway.
func publicUsageSink(batch *publicUsageBatch) func(context.Context, gateway.UsageSample) {
	if batch == nil {
		return nil
	}
	return func(ctx context.Context, s gateway.UsageSample) {
		// Grant identity comes from the peer-auth middleware
		// (internal/inference wgPeerOnly), which stamps the whole
		// PeerIdentity — grant included — as the OUTERMOST layer, so it
		// is present here for every overlay request.
		peer, ok := inference.PeerFromContext(ctx)
		if !ok || !peer.IsPublicConsumer() || peer.Grant == nil || peer.Grant.ID == "" {
			// Not a Public Share guest: an own-account mesh peer's usage
			// is not reported to the control plane.
			return
		}
		batch.Record(peer.Grant.ID, usageReportModelID(s), s.Class,
			s.InputTokens, s.OutputTokens, s.DurationMS)
	}
}

// usageReportModelID picks the identifier the CP can resolve a quality
// tier from.
//
// The CP calls proto/catalog.BestTier on this string, which matches
// Variant.Source.Tag (ollama) / Source.RepoID (vLLM). UsageSample
// already carries the engine-native name rather than the catalog id for
// exactly that reason; the fallback to ModelID only matters for a
// selection built without one.
func usageReportModelID(s gateway.UsageSample) string {
	if s.EngineModel != "" {
		return s.EngineModel
	}
	return s.ModelID
}
