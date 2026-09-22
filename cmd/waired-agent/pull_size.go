package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/ollamaregistry"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
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
//
// It returns the total, 0 when unknown, for the disk check that follows.
func (p *agentInferenceProvider) seedPullTotal(ctx context.Context, modelID, tag string) int64 {
	if p == nil || p.dlProgress == nil || tag == "" {
		return 0
	}
	sizeCtx, cancel := context.WithTimeout(ctx, pullSizeBudget)
	defer cancel()
	total, err := tagSizeFn(sizeCtx, tag)
	if err != nil {
		p.logger.Debug("could not read the pull's total size from the registry; the bar will add it up as it goes",
			"model", modelID, "tag", tag, "err", err)
		return 0
	}
	p.dlProgress.seedTotal(modelID, total)
	return total
}

// freeDiskFn is the seam tests swap; sealed in TestMain, because the real
// one reads this machine's disk.
var freeDiskFn = hardware.FreeDiskBytes

// diskShortfall is the failure to record when the filesystem holding the
// engine's models has less free space than the pull's whole size; ""
// means go ahead, and so does anything unknown — the size, the store, the
// free space. It runs before a byte is fetched, for bundled and imported
// models alike (waired-ai/waired#1480): a multi-GB download that ends in a
// full disk costs the time and leaves the disk full.
//
// The whole size, not the size still to fetch: a pull resumed after a
// restart already holds part of it and may be refused while it would have
// fitted. The message names both figures, and the fix is the same.
func (p *agentInferenceProvider) diskShortfall(total int64) string {
	if p == nil {
		return ""
	}
	return diskShortfallAt(p.ollamaModelsDir, total)
}

// diskShortfallAt is diskShortfall for the filesystem holding dir: the
// engine's model store for ollama, the weights root for vLLM.
func diskShortfallAt(dir string, total int64) string {
	if total <= 0 || dir == "" {
		return ""
	}
	free, err := freeDiskFn(dir)
	if err != nil {
		return ""
	}
	if err := download.CheckDiskSpace(free, total); err != nil {
		// "insufficient disk space" is one of diskFullMarkers, so setup
		// reports this as disk_full, as it does the engine's own ENOSPC.
		// "only … is free" is the shape the vLLM rebuild check already uses
		// (runtime.VLLMConvergeDecision), so the two disk refusals read alike.
		return "insufficient disk space for this model: it needs " + download.HumanBytes(total) +
			" and only " + download.HumanBytes(free) + " is free"
	}
	return ""
}

// errDiskShort is a pull refused before it started for want of disk.
var errDiskShort = errors.New("insufficient disk space for this model")

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
