package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
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

// tagDigestFn is the seam tests swap for the registry digest read; sealed
// in TestMain like tagSizeFn.
var tagDigestFn = func(ctx context.Context, tag string) (string, error) {
	c := &ollamaregistry.Client{}
	return c.TagDigest(ctx, tag)
}

// errSourceChanged is a pull refused because the tag no longer names the
// build the catalog was measured against (waired-agent#1305).
var errSourceChanged = errors.New("the published build behind this tag has changed")

// sourceChangedFailure checks a pinned tag against the registry before a
// byte is fetched, and returns the failure to record when the registry
// now serves a different manifest under it; "" means go ahead.
//
// A tag is a name, and a community namespace can push other weights under
// it: frob/qwen3.8-flash-next went from 55 GB to 79 GB with the name
// unchanged, and every size, fit and download-time figure Waired showed
// for it became wrong with no signal (#1305). Downloading the new build
// anyway would serve weights no one has measured on a host that was told
// they fit, so the pull stops and says why.
//
// An unpinned variant, and a registry that cannot answer, go ahead: the
// pin only protects a build Waired recorded, and a read that failed says
// nothing about whether it moved — a pull that fetches from the same
// registry will find out for itself.
func (p *agentInferenceProvider) sourceChangedFailure(ctx context.Context, modelID string, v catalog.Variant) string {
	if v.Source.Digest == "" || v.Source.Tag == "" {
		return ""
	}
	digCtx, cancel := context.WithTimeout(ctx, pullSizeBudget)
	defer cancel()
	got, err := tagDigestFn(digCtx, v.Source.Tag)
	if err != nil {
		p.logger.Info("could not read the registry digest of a pinned tag; pulling without the check",
			"model", modelID, "tag", v.Source.Tag, "err", err)
		return ""
	}
	if got == v.Source.Digest {
		return ""
	}
	p.logger.Warn("refusing to pull: the registry now serves a different build under this tag",
		"model", modelID, "tag", v.Source.Tag, "pinned", v.Source.Digest, "registry", got)
	return fmt.Sprintf("%s: %s is no longer the build Waired lists (registry %s, catalog %s). Update Waired to get a catalog that knows it",
		errSourceChanged, v.Source.Tag, got, v.Source.Digest)
}
