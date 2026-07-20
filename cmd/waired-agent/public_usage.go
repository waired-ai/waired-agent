package main

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Provider-side Public Share usage reporting (waired#829, spec §12).
//
// The gateway hands one UsageSample per served request to the sink in
// public_usage_sink.go, which aggregates here; this loop flushes the
// aggregate to the control plane. Shaped after runPublicGrantLoop: a
// narrow API seam, nil-able clock and logger, all state goroutine- or
// mutex-local, and its own wg.Add.
//
// Only aggregate counters ever leave this file. There is no path by
// which prompt or message content could reach it (spec §15-10).

const (
	// publicUsageFlushInterval is the periodic flush cadence (spec §12).
	publicUsageFlushInterval = 60 * time.Second
	// publicUsageFlushKeys forces an early flush once this many distinct
	// (grant, model, class) triples are pending.
	publicUsageFlushKeys = 50
	// publicUsageMaxKeys bounds memory when the CP is unreachable for a
	// long time. Beyond it, new triples are dropped rather than grown
	// without limit; existing triples keep accumulating.
	publicUsageMaxKeys = 512
	// publicUsageMaxEntries / publicUsageMaxBodyBytes mirror the CP's
	// intake caps, so a flush is chunked rather than rejected whole.
	publicUsageMaxEntries   = 200
	publicUsageMaxBodyBytes = 100 << 10
	// publicUsagePushTimeout bounds one CP round trip. controlclient's
	// HTTP client has no timeout by design, so without this a blackholed
	// connection parks the loop until the OS gives up.
	publicUsagePushTimeout = 10 * time.Second
	// publicUsageMinPushInterval paces chunks inside one flush: the CP
	// allows 1 report / 5s with burst 5.
	publicUsageMinPushInterval = 6 * time.Second
	// publicUsageMaxChunks caps how many chunks one flush may send, so a
	// huge backlog cannot monopolise the loop.
	publicUsageMaxChunks = 5
)

// publicUsageAPI is the controlclient seam (fake in tests).
type publicUsageAPI interface {
	PushPublicUsage(ctx context.Context, deviceID string, report signer.PublicUsageReport, machineKey ed25519.PrivateKey) (controlclient.PublicUsagePushResult, error)
}

// usageKey is the aggregation triple the CP's ledger is keyed on.
type usageKey struct {
	grantID string
	modelID string
	class   string
}

// usageAgg accumulates one key's counters over one window.
type usageAgg struct {
	requests     int64
	inputTokens  int64
	outputTokens int64
	inferenceMS  int64
	windowStart  time.Time
	windowEnd    time.Time
}

// publicUsageBatch is the mutex-guarded accumulator the gateway sink
// writes into.
//
// The mutex is held only inside Record, drain and requeue — never
// across a CP round trip or the inter-chunk pacing sleep. A serving
// request must never block on the control plane.
type publicUsageBatch struct {
	mu      sync.Mutex
	agg     map[usageKey]*usageAgg
	dropped int64

	now  func() time.Time
	kick chan struct{}
}

func newPublicUsageBatch(now func() time.Time) *publicUsageBatch {
	if now == nil {
		now = time.Now
	}
	return &publicUsageBatch{
		agg:  map[usageKey]*usageAgg{},
		now:  now,
		kick: make(chan struct{}, 1),
	}
}

// Record folds one served request into the batch. Called on the :9474
// request path, so it does no I/O and never blocks.
func (b *publicUsageBatch) Record(grantID, modelID, class string, inTok, outTok, durMS int64) {
	if b == nil || grantID == "" || modelID == "" {
		return
	}
	k := usageKey{grantID: grantID, modelID: modelID, class: class}
	ts := b.now().UTC()

	b.mu.Lock()
	a, ok := b.agg[k]
	if !ok {
		if len(b.agg) >= publicUsageMaxKeys {
			b.dropped++
			b.mu.Unlock()
			return
		}
		a = &usageAgg{windowStart: ts}
		b.agg[k] = a
	}
	a.requests++
	a.inputTokens += inTok
	a.outputTokens += outTok
	a.inferenceMS += durMS
	a.windowEnd = ts
	n := len(b.agg)
	b.mu.Unlock()

	if n >= publicUsageFlushKeys {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

// drain removes and returns every pending entry.
func (b *publicUsageBatch) drain() ([]signer.PublicUsageEntry, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.agg) == 0 {
		dropped := b.dropped
		b.dropped = 0
		return nil, dropped
	}
	out := make([]signer.PublicUsageEntry, 0, len(b.agg))
	for k, a := range b.agg {
		out = append(out, signer.PublicUsageEntry{
			GrantID:      k.grantID,
			ModelID:      k.modelID,
			Class:        k.class,
			Requests:     a.requests,
			InputTokens:  a.inputTokens,
			OutputTokens: a.outputTokens,
			InferenceMS:  a.inferenceMS,
			WindowStart:  a.windowStart.Format(time.RFC3339),
			WindowEnd:    a.windowEnd.Format(time.RFC3339),
		})
	}
	b.agg = map[usageKey]*usageAgg{}
	dropped := b.dropped
	b.dropped = 0
	return out, dropped
}

// requeue folds provably-unsent entries back in, so a rate-limited or
// clock-skewed flush is not lost. Counters merge rather than overwrite,
// because Record may have added more for the same key meanwhile.
func (b *publicUsageBatch) requeue(entries []signer.PublicUsageEntry) {
	if len(entries) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range entries {
		k := usageKey{grantID: e.GrantID, modelID: e.ModelID, class: e.Class}
		a, ok := b.agg[k]
		if !ok {
			if len(b.agg) >= publicUsageMaxKeys {
				b.dropped++
				continue
			}
			a = &usageAgg{}
			b.agg[k] = a
		}
		a.requests += e.Requests
		a.inputTokens += e.InputTokens
		a.outputTokens += e.OutputTokens
		a.inferenceMS += e.InferenceMS
		if ws, err := time.Parse(time.RFC3339, e.WindowStart); err == nil {
			if a.windowStart.IsZero() || ws.Before(a.windowStart) {
				a.windowStart = ws
			}
		}
		if we, err := time.Parse(time.RFC3339, e.WindowEnd); err == nil && we.After(a.windowEnd) {
			a.windowEnd = we
		}
	}
}

type publicUsageDeps struct {
	API        publicUsageAPI
	Batch      *publicUsageBatch
	DeviceID   string
	MachineKey ed25519.PrivateKey
	Logger     *slog.Logger

	Interval time.Duration    // 0 → publicUsageFlushInterval
	Now      func() time.Time // nil → time.Now
	// MinPushInterval paces chunks within one flush; 0 →
	// publicUsageMinPushInterval. Tests shorten it.
	MinPushInterval time.Duration
}

// runPublicUsageLoop flushes the batch periodically, on a size kick, and
// once more at shutdown.
func runPublicUsageLoop(ctx context.Context, deps publicUsageDeps) {
	if deps.API == nil || deps.Batch == nil || deps.DeviceID == "" ||
		len(deps.MachineKey) != ed25519.PrivateKeySize {
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := deps.Interval
	if interval <= 0 {
		interval = publicUsageFlushInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final flush on a fresh context: the session context is
			// already cancelled, and the work being reported is done.
			// Budgeted for the worst case this flush can produce —
			// every chunk paced and timing out — so a post-outage
			// backlog is not silently discarded.
			budget := publicUsageMinPushInterval*(publicUsageMaxChunks-1) + publicUsagePushTimeout*publicUsageMaxChunks
			fctx, cancel := context.WithTimeout(context.Background(), budget)
			flushPublicUsage(fctx, deps, logger)
			cancel()
			return
		case <-ticker.C:
		case <-deps.Batch.kick:
		}
		flushPublicUsage(ctx, deps, logger)
	}
}

// flushPublicUsage sends everything pending, chunked to the CP's caps
// and paced to its rate limit.
func flushPublicUsage(ctx context.Context, deps publicUsageDeps, logger *slog.Logger) {
	entries, dropped := deps.Batch.drain()
	if dropped > 0 {
		// Never silent: a dropped entry is usage the user served and
		// will not be credited for.
		logger.Warn("public usage: entries dropped (accumulator full)", "count", dropped)
	}
	if len(entries) == 0 {
		return
	}
	minGap := deps.MinPushInterval
	if minGap <= 0 {
		minGap = publicUsageMinPushInterval
	}

	for chunkIdx := 0; len(entries) > 0; chunkIdx++ {
		if chunkIdx >= publicUsageMaxChunks {
			// Keep the remainder for the next flush rather than
			// hammering the CP or blocking here indefinitely.
			deps.Batch.requeue(entries)
			logger.Info("public usage: deferring remainder to the next flush", "entries", len(entries))
			return
		}
		if chunkIdx > 0 {
			if !sleepCtx(ctx, minGap) {
				deps.Batch.requeue(entries)
				return
			}
		}
		chunk, rest := splitUsageChunk(entries)
		entries = rest

		pctx, cancel := context.WithTimeout(ctx, publicUsagePushTimeout)
		_, err := deps.API.PushPublicUsage(pctx, deps.DeviceID,
			signer.PublicUsageReport{Entries: chunk}, deps.MachineKey)
		cancel()
		if err == nil {
			continue
		}

		var ue *controlclient.PublicUsageError
		switch {
		case errorsAsPublicUsage(err, &ue) && ue.Retryable():
			// Provably never written — safe to send again later.
			deps.Batch.requeue(chunk)
			deps.Batch.requeue(entries)
			logger.Info("public usage: report deferred", "status", ue.Status, "code", ue.Code)
			return
		case ue != nil:
			// Definitive rejection: re-sending cannot help.
			logger.Warn("public usage: report rejected", "status", ue.Status, "code", ue.Code,
				"entries", len(chunk))
			continue
		default:
			// Transport failure: whether the CP wrote the rows is
			// unknowable, and the endpoint is not idempotent, so the
			// chunk is dropped. Under-counting beats double-counting.
			logger.Warn("public usage: report failed", "err", err, "entries", len(chunk))
			continue
		}
	}
}

// splitUsageChunk peels off the largest prefix within both the CP's
// entry-count cap and its body-size cap.
func splitUsageChunk(entries []signer.PublicUsageEntry) (chunk, rest []signer.PublicUsageEntry) {
	n := len(entries)
	if n > publicUsageMaxEntries {
		n = publicUsageMaxEntries
	}
	// Conservative per-entry size bound; the entries are small, fixed
	// shapes, so a byte estimate avoids marshalling twice.
	const approxEntryBytes = 256
	if max := publicUsageMaxBodyBytes / approxEntryBytes; n > max {
		n = max
	}
	if n < 1 {
		n = 1
	}
	return entries[:n], entries[n:]
}

// sleepCtx waits d, or returns false if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errorsAsPublicUsage is errors.As specialised to the typed CP error.
func errorsAsPublicUsage(err error, target **controlclient.PublicUsageError) bool {
	if e, ok := err.(*controlclient.PublicUsageError); ok {
		*target = e
		return true
	}
	return false
}
