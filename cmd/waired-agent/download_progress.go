package main

import (
	"sync"

	"github.com/waired-ai/waired-agent/internal/download"
)

// downloadProgress tracks live byte progress for in-flight model pulls.
//
// `ollama pull` streams progress per *layer* (each with its own digest +
// completed/total bytes); the overall download size a user wants to see
// ("2.3 GB / 5.0 GB") is the sum across all layers. We therefore keep the
// latest completed/total per (model, digest) and aggregate on read. State
// is transient and in-memory only — it is never persisted to state.json.
type downloadProgress struct {
	mu sync.Mutex
	// modelID -> layer digest -> latest byte counts for that layer.
	layers map[string]map[string]layerBytes
}

type layerBytes struct {
	completed int64
	total     int64
	// rateBps is the layer's last reported speed. Kept per layer for the
	// same reason the byte counts are: Ollama prints one rate per layer,
	// and the figure a reader wants is what the download as a whole is
	// moving at.
	rateBps int64
}

func newDownloadProgress() *downloadProgress {
	return &downloadProgress{layers: map[string]map[string]layerBytes{}}
}

// observe records one progress event for modelID. Non-layer lines (empty
// digest) and size-less updates are ignored so they don't clobber a known
// layer total with zero.
func (d *downloadProgress) observe(modelID string, pr download.Progress) {
	if d == nil || pr.Digest == "" || pr.Total <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.layers[modelID]
	if m == nil {
		m = map[string]layerBytes{}
		d.layers[modelID] = m
	}
	m[pr.Digest] = layerBytes{completed: pr.Completed, total: pr.Total, rateBps: pr.BytesPerSec}
}

// aggregate returns the summed completed/total bytes across modelID's
// layers, and what they are moving at. ok is false when nothing is known
// yet (no size-bearing layer line seen), so callers can omit the model
// rather than show "0 / 0".
//
// rateBps sums only the layers still short of their total. A finished
// layer keeps the last speed it reported, and adding those back in would
// make a download read faster the more of it was already done — by the
// final layer, the figure would be mostly the memory of layers that
// stopped moving minutes ago. 0 means "nothing is known to be moving",
// which the wire and the console both already treat as unknown; a stall
// is completed_bytes not advancing, never a zero rate (waired#1286).
func (d *downloadProgress) aggregate(modelID string) (completed, total, rateBps int64, ok bool) {
	if d == nil {
		return 0, 0, 0, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.layers[modelID]
	if len(m) == 0 {
		return 0, 0, 0, false
	}
	for _, lb := range m {
		completed += lb.completed
		total += lb.total
		if lb.completed < lb.total {
			rateBps += lb.rateBps
		}
	}
	return completed, total, rateBps, total > 0
}

// forget drops modelID's progress once its pull terminates (success or
// failure) so a finished/failed model never lingers as "downloading".
func (d *downloadProgress) forget(modelID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.layers, modelID)
}
