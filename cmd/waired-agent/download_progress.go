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
// The total, though, is not a sum of what has been seen. A puller
// announces a layer when it reaches it, so a total added up from the
// progress lines starts at the first layer's size and steps up each time
// another begins — a bar that fills to 100 % of 0.9 GB and then restarts
// against 17.7 GB, which is what an operator reads as the download
// starting over (waired-agent#1299). seedTotal takes the whole figure
// from the source that knows it before the first byte moves.
type downloadProgress struct {
	mu sync.Mutex
	// modelID -> layer digest -> latest byte counts for that layer.
	layers map[string]map[string]layerBytes
	// modelID -> the pull's whole size, when the caller knew it up front.
	seeded map[string]int64
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
	return &downloadProgress{
		layers: map[string]map[string]layerBytes{},
		seeded: map[string]int64{},
	}
}

// seedTotal records how many bytes modelID's pull has to fetch in all,
// learned before it started. Callers that cannot find out say nothing and
// the aggregate falls back to summing the layers it has seen — a total
// that climbs is worse than one that is right, but it beats refusing to
// draw a bar because a registry was unreachable.
//
// Only a positive figure seeds: 0 would read as "nothing to download".
// A later seed replaces an earlier one rather than adding to it.
func (d *downloadProgress) seedTotal(modelID string, total int64) {
	if d == nil || total <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seeded[modelID] = total
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
	// A seeded total wins while the layers seen so far add up to less than
	// it — which is the whole span of the download bar's life, and the
	// point of seeding. max rather than an outright replacement: if the
	// layers ever exceed the figure the manifest gave, what is actually
	// being fetched is the truth, and a bar past 100 % is worse than a
	// total that grew once.
	if seeded := d.seeded[modelID]; seeded > total {
		total = seeded
	}
	// completed can only exceed total if the two disagree; clamping keeps
	// the percentage the callers compute inside its range.
	if completed > total {
		completed = total
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
	delete(d.seeded, modelID)
}
