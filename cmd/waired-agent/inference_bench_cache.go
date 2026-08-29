package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// benchCacheSchemaVersion gates forward-compat parsing. The cache is
// purely an optimisation, so an unknown version is treated as a miss
// (existing file is overwritten on the next store). Bump this whenever
// benchCacheEntry's shape changes in a way that would mis-interpret
// older data, OR when the measurement semantics change enough that
// old numbers must not be trusted.
//
// v2: measurements are taken after a warm-up request, so model-load
// latency is no longer inside the measured window. v1 entries on
// cold-booted hosts under-reported tok/s by up to ~20× and must be
// re-measured.
//
// v3 (#764): TokensPerSec is a decode rate — ollama eval counters or
// the two-length slope, median of benchSampleCount runs. v2 single-run
// wall-clock numbers understate fast hosts ~35% (fixed ~1.4 s request
// overhead attributed to decode) and must be re-measured.
//
// v4 (#1131): every key now carries the engine RELEASE, not just the
// engine kind. Adding it to the key already stops old entries from
// matching, but they would linger as unreachable JSON; the bump drops
// them. It lands with the pin move that first makes this cost anything
// — before it, no installed host had ever changed engine version
// without also changing agent build.
const benchCacheSchemaVersion = 4

// benchCacheFile is the on-disk form of the boot benchmark cache.
// Lives at $XDG_CACHE_HOME/waired/bench.json (or ~/.cache/waired/bench.json).
// Multiple entries coexist so the same machine can serve different
// (variant, engine_kind, engine_model) combinations without
// recomputing — each entry is keyed by the sha256 returned by
// benchCacheKey.
type benchCacheFile struct {
	Version int                        `json:"version"`
	Entries map[string]benchCacheEntry `json:"entries"`
	// Depth holds the #624 long-context sweeps, keyed by
	// depthBenchCacheKey (adds applied window + KV type to the boot
	// key). Additive to schema v2: older binaries drop the map on
	// their next Store, which only costs a re-measure.
	Depth map[string]depthCacheEntry `json:"depth,omitempty"`
}

// benchCacheEntry is one cached measurement. The identifying fields
// (GPUModel/VRAMTotalMB/DriverVersion/VariantID/EngineKind/EngineModel)
// are duplicated from the cache key for human auditability — they let
// an operator open bench.json in a text editor and see what was
// measured without reversing the sha256.
type benchCacheEntry struct {
	TokensPerSec  float64 `json:"tokens_per_sec"`
	Capacity      int     `json:"capacity"`
	VariantID     string  `json:"variant_id"`
	GPUModel      string  `json:"gpu_model"`
	VRAMTotalMB   int     `json:"vram_total_mb,omitempty"`
	DriverVersion string  `json:"driver_version,omitempty"`
	EngineKind    string  `json:"engine_kind"`
	EngineModel   string  `json:"engine_model,omitempty"`
	// EngineVersion is the release of EngineKind this entry was
	// measured on. Never empty in a stored entry — an unreadable
	// version disables caching in benchCacheKey — so it has no
	// omitempty: a blank here would mean the file was hand-edited.
	EngineVersion string `json:"engine_version"`
	// Method / SpreadPct mirror BenchResult (#764) so an operator can
	// see how a cached number was measured and how noisy it was.
	Method     string    `json:"method,omitempty"`
	SpreadPct  float64   `json:"spread_pct,omitempty"`
	MeasuredAt time.Time `json:"measured_at"`
}

// benchCacheHumanMeta carries the identifying inputs that get embedded
// in the cache entry for human readability. Distinct type from
// BenchDeps to keep the cache module decoupled from BenchDeps' wider
// surface (e.g. Logger / HTTPClient have no business in the cache).
type benchCacheHumanMeta struct {
	VariantID     string
	GPUModel      string
	VRAMTotalMB   int
	DriverVersion string
	EngineKind    string
	EngineModel   string
	EngineVersion string
}

// benchCache is the file-backed boot benchmark cache. The zero value
// is unusable; construct via newBenchCache. A nil *benchCache means
// "no cache" — Load returns (_, _, false, nil) and Store no-ops, so
// callers do not need nil checks.
type benchCache struct {
	path   string
	logger *slog.Logger
}

func newBenchCache(path string, logger *slog.Logger) *benchCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &benchCache{path: path, logger: logger}
}

// benchCacheKey composes the cache key from the inputs that affect
// measured token/s. Empty when GPUModel, VariantSHA or EngineVersion is
// missing — this disables caching for CPU-only hosts, for variants whose
// digest could not be computed, and for engines whose version could not
// be read. The first two would produce undifferentiable keys across
// machines; the third would produce one that survives an engine upgrade
// (#1131), which is the whole defect.
//
// Refusing to cache on an unreadable version is the same fail-closed
// rule hostSpeedStillApplies (host_cutoff.go) and
// router.engineVersionSatisfies already apply: an engine whose version
// cannot be read is not evidence that it is current. It costs a
// re-measure, never a wrong number.
func benchCacheKey(d BenchDeps) string {
	if d.GPUModel == "" || d.VariantSHA == "" || d.EngineVersion == "" {
		return ""
	}
	h := sha256.New()
	// hash.Hash.Write never errors; the discard satisfies errcheck
	// without obscuring the format string at the call site.
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s",
		d.GPUModel, d.VRAMTotalMB, d.DriverVersion,
		d.VariantSHA, d.EngineKind, d.EngineModel, d.EngineVersion)
	return hex.EncodeToString(h.Sum(nil))
}

// benchCacheDisabledReason names, in the operator's terms, why
// benchCacheKey / depthBenchCacheKey would answer "". Empty string means
// caching is on.
//
// It exists because the two guards that read an empty key — the Load at
// the top of RunBootBenchmark and the Store at the bottom — both skip
// without a line of output, so a host that has never cached anything and
// a host whose cache simply has not been reached look identical in the
// journal. Working out which of the three inputs was missing on a live
// vLLM host took a session of log archaeology (waired-agent#1150).
//
// All three are reported when all three are missing: on a fresh install
// they can go missing together, and fixing one at a time would take three
// boots to learn.
func benchCacheDisabledReason(gpuModel, variantSHA, engineVersion string) string {
	var why []string
	if gpuModel == "" {
		why = append(why, "no GPU was detected on this host")
	}
	if variantSHA == "" {
		// variantSHAForActive answers "" for a selection the bundled
		// catalog does not carry, as well as for no selection at all.
		why = append(why, "the active model is not in this build's catalog")
	}
	if engineVersion == "" {
		why = append(why, "the engine version could not be read")
	}
	return strings.Join(why, "; ")
}

// Load returns a previously-cached measurement for key. hit=false when
// the cache is absent, the key is missing, or the file is unparseable
// (the latter is warn-logged so an operator can investigate; the
// benchmark then re-measures and overwrites on the next store).
//
// A non-nil err is returned only for filesystem errors other than
// ENOENT — those propagate so the caller can decide whether to
// continue (RunBootBenchmark currently treats them as misses too).
func (c *benchCache) Load(key string) (BenchResult, time.Time, bool, error) {
	if c == nil || key == "" {
		return BenchResult{}, time.Time{}, false, nil
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BenchResult{}, time.Time{}, false, nil
		}
		return BenchResult{}, time.Time{}, false, fmt.Errorf("read bench cache: %w", err)
	}
	var file benchCacheFile
	if err := json.Unmarshal(raw, &file); err != nil {
		c.logger.Warn("bench cache: file unparseable, ignoring", "path", c.path, "err", err)
		return BenchResult{}, time.Time{}, false, nil
	}
	if file.Version != benchCacheSchemaVersion {
		c.logger.Warn("bench cache: schema version mismatch, ignoring",
			"path", c.path, "got", file.Version, "want", benchCacheSchemaVersion)
		return BenchResult{}, time.Time{}, false, nil
	}
	entry, ok := file.Entries[key]
	if !ok {
		return BenchResult{}, time.Time{}, false, nil
	}
	return BenchResult{
		TokensPerSec: entry.TokensPerSec,
		Capacity:     entry.Capacity,
		VariantID:    entry.VariantID,
		Method:       entry.Method,
		SpreadPct:    entry.SpreadPct,
	}, entry.MeasuredAt, true, nil
}

// Store persists a measurement under key. The write is atomic via
// rename(2) of a sibling .tmp file. Existing entries under other keys
// are preserved (re-merged from the on-disk file) so that switching
// variants does not blow away the cache for the previous one.
//
// Errors that don't otherwise prevent the agent from running (cache
// directory uncreatable, write fails) are returned to the caller —
// RunBootBenchmark warn-logs them but proceeds.
func (c *benchCache) Store(key string, r BenchResult, meta benchCacheHumanMeta, now time.Time) error {
	if c == nil || key == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("mkdir bench cache: %w", err)
	}
	// Read-modify-write so cache entries for variants we are not
	// measuring this boot survive.
	var file benchCacheFile
	if raw, err := os.ReadFile(c.path); err == nil {
		if jerr := json.Unmarshal(raw, &file); jerr != nil {
			// File exists but corrupt — start fresh; the offending
			// entry would otherwise re-trigger the warn-log each boot.
			file = benchCacheFile{}
		}
	}
	if file.Version != benchCacheSchemaVersion {
		// A schema bump invalidates old measurements; carrying their
		// entries into the new file would resurface them as fresh
		// cache hits for other variants. Start over.
		file = benchCacheFile{Version: benchCacheSchemaVersion}
	}
	if file.Entries == nil {
		file.Entries = make(map[string]benchCacheEntry)
	}
	file.Entries[key] = benchCacheEntry{
		TokensPerSec:  r.TokensPerSec,
		Capacity:      r.Capacity,
		VariantID:     meta.VariantID,
		GPUModel:      meta.GPUModel,
		VRAMTotalMB:   meta.VRAMTotalMB,
		DriverVersion: meta.DriverVersion,
		EngineKind:    meta.EngineKind,
		EngineModel:   meta.EngineModel,
		EngineVersion: meta.EngineVersion,
		Method:        r.Method,
		SpreadPct:     r.SpreadPct,
		MeasuredAt:    now,
	}
	buf, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bench cache: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("write bench cache tmp: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("rename bench cache: %w", err)
	}
	return nil
}

// Invalidate removes the cache file unconditionally. ENOENT is not an
// error. Used by the --bench-cache-invalidate startup flag.
func (c *benchCache) Invalidate() error {
	if c == nil {
		return nil
	}
	if err := os.Remove(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove bench cache: %w", err)
	}
	return nil
}

// defaultWairedCachePath returns the on-disk location of the boot
// benchmark cache, honouring XDG_CACHE_HOME and falling back to the
// user home (os.UserHomeDir, so %USERPROFILE% works on Windows).
// Returns "" when no home resolves (test runners, containers without
// a HOME); the caller should treat that as "caching disabled" rather
// than writing to /tmp.
func defaultWairedCachePath() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "waired", "bench.json")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".cache", "waired", "bench.json")
	}
	return ""
}

// --- Depth benchmark cache (#624) -------------------------------------------

// depthCacheEntry is one cached depth sweep. The GPU fields duplicate
// the key inputs for human auditability (same convention as
// benchCacheEntry); the sweep itself carries variant / window / KV type.
type depthCacheEntry struct {
	Result        DepthBenchResult `json:"result"`
	GPUModel      string           `json:"gpu_model"`
	VRAMTotalMB   int              `json:"vram_total_mb,omitempty"`
	DriverVersion string           `json:"driver_version,omitempty"`
	EngineVersion string           `json:"engine_version"`
}

// depthBenchCacheKey extends the boot-bench key with the applied
// context window and KV type — the tuning inputs that change what a
// depth sweep measures. Empty when GPUModel, VariantSHA or
// EngineVersion is missing (same rationale as benchCacheKey).
//
// The engine version belongs here for the same reason it belongs on the
// boot key, and with more at stake: PrefillTokps is the only per-model
// prefill measurement this product takes, #1127 publishes it, and
// prefill is precisely what an engine release moves — 0.33.0 reworked
// ollama's prefill restore points outright.
//
// The generation ubatch was in this key too, so a 512 sweep and a
// forced-2048 sweep could not collide. Retiring that override
// (waired-agent#1079) left the engine to size its own batch from inputs
// this key already carries — the window and the model — so the term went
// with it. Every existing depth entry misses once and re-measures, which
// is the correct outcome: they were taken under a configuration this
// host no longer serves.
func depthBenchCacheKey(d DepthBenchDeps) string {
	if d.GPUModel == "" || d.VariantSHA == "" || d.EngineVersion == "" {
		return ""
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "depth\x00%s\x00%d\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s",
		d.GPUModel, d.VRAMTotalMB, d.DriverVersion,
		d.VariantSHA, d.EngineModel, d.ContextLength, d.KVCacheType, d.EngineVersion)
	return hex.EncodeToString(h.Sum(nil))
}

// LoadDepth returns a previously-cached depth sweep for key. Miss
// semantics mirror Load.
func (c *benchCache) LoadDepth(key string) (DepthBenchResult, bool, error) {
	if c == nil || key == "" {
		return DepthBenchResult{}, false, nil
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DepthBenchResult{}, false, nil
		}
		return DepthBenchResult{}, false, fmt.Errorf("read bench cache: %w", err)
	}
	var file benchCacheFile
	if err := json.Unmarshal(raw, &file); err != nil {
		c.logger.Warn("bench cache: file unparseable, ignoring", "path", c.path, "err", err)
		return DepthBenchResult{}, false, nil
	}
	if file.Version != benchCacheSchemaVersion {
		return DepthBenchResult{}, false, nil
	}
	entry, ok := file.Depth[key]
	if !ok {
		return DepthBenchResult{}, false, nil
	}
	return entry.Result, true, nil
}

// StoreDepth persists a COMPLETED depth sweep under key (incomplete
// runs are the caller's responsibility to withhold — a partial sweep
// re-measures next boot). Atomic rename, other entries preserved.
func (c *benchCache) StoreDepth(key string, r DepthBenchResult, gpuModel string, vramTotalMB int, driverVersion, engineVersion string) error {
	if c == nil || key == "" {
		return nil
	}
	if !r.Completed {
		return fmt.Errorf("refusing to cache an incomplete depth sweep")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("mkdir bench cache: %w", err)
	}
	var file benchCacheFile
	if raw, err := os.ReadFile(c.path); err == nil {
		if jerr := json.Unmarshal(raw, &file); jerr != nil {
			file = benchCacheFile{}
		}
	}
	if file.Version != benchCacheSchemaVersion {
		file = benchCacheFile{Version: benchCacheSchemaVersion}
	}
	if file.Entries == nil {
		file.Entries = make(map[string]benchCacheEntry)
	}
	if file.Depth == nil {
		file.Depth = make(map[string]depthCacheEntry)
	}
	file.Depth[key] = depthCacheEntry{
		Result:        r,
		GPUModel:      gpuModel,
		VRAMTotalMB:   vramTotalMB,
		DriverVersion: driverVersion,
		EngineVersion: engineVersion,
	}
	buf, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bench cache: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("write bench cache tmp: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("rename bench cache: %w", err)
	}
	return nil
}
