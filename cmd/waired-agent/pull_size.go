package main

import (
	"context"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/ollamaregistry"
)

// pullSizeBudget bounds the one registry request a pull makes before it
// starts. Short on purpose: the answer only improves the download bar, so
// a slow or unreachable registry must cost the download seconds, not
// minutes. The pull proceeds either way.
const pullSizeBudget = 10 * time.Second

// tagSizeFn is the seam tests swap. The real one is exercised by
// ollamaregistry's own table tests (internal/catalog/ollamaregistry).
var tagSizeFn = func(ctx context.Context, tag string) (int64, error) {
	c := &ollamaregistry.Client{}
	return c.TagSize(ctx, tag)
}

// seedPullTotal tells the progress tracker how big tag's pull is before
// the first byte moves.
//
// Without it the bar's total is the sum of the layers ollama has
// announced so far, and ollama announces a layer when it reaches it — so
// a tag whose projector layer comes first shows a bar that fills against
// 0.9 GB and then restarts against 17.7 GB (waired-agent#1299; owner
// ruling 2026-09-12 that the bar is one bar). The manifest carries every
// layer's size, and both registries an ollama tag can name serve it at
// the same path.
//
// Failures are logged and dropped. There is nothing to recover: the
// aggregate falls back to summing what it sees, which is what every
// release before this one did.
func (p *agentInferenceProvider) seedPullTotal(ctx context.Context, modelID, tag string) {
	if p == nil || p.dlProgress == nil || tag == "" {
		return
	}
	sizeCtx, cancel := context.WithTimeout(ctx, pullSizeBudget)
	defer cancel()
	total, err := tagSizeFn(sizeCtx, tag)
	if err != nil {
		p.logger.Debug("could not read the pull's total size from the registry; the bar will add it up as it goes",
			"model", modelID, "tag", tag, "err", err)
		return
	}
	p.dlProgress.seedTotal(modelID, total)
}
