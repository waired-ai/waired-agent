package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// StateVersion is the schema version stored in state.json. Bump and
// add a migration path whenever the on-disk shape changes.
//
// History:
//
//	v1 (Phase A): models + endpoints only.
//	v2 (Step 2):  adds active, runtimes, external_manifests + extends
//	              ModelState with hf_repo / local_path.
const StateVersion = 2

// Model lifecycle states (spec waired_inference_spec.md §9.3).
const (
	ModelStateNotPresent  = "not_present"
	ModelStateQueued      = "queued"
	ModelStateDownloading = "downloading"
	ModelStateVerifying   = "verifying"
	ModelStateReady       = "ready"
	ModelStateFailed      = "failed"
	ModelStateEvicted     = "evicted"
)

// State is the persisted view of the local inference cache.
//
// Active, Runtimes, and ExternalManifests are v2 additions; v1 files
// unmarshal into a State with those fields nil/empty (the JSON shape
// is forward-compatible). MigrateInPlace fills Active and bumps
// Version when a v1 file is loaded.
type State struct {
	Version           int                       `json:"version"`
	Active            *ActiveSelection          `json:"active,omitempty"`
	Models            map[string]ModelState     `json:"models"`
	Runtimes          map[string]RuntimeInstall `json:"runtimes,omitempty"`
	Endpoints         map[string]EndpointState  `json:"endpoints"`
	ExternalManifests []ExternalManifestRef     `json:"external_manifests,omitempty"`

	// DismissedRecommendations records benchmark step-down suggestions
	// (issue #133) the user declined, so a re-benchmark of the same
	// pairing does not re-nag. Keyed by DismissalKey(activeVariantSHA,
	// toVariantID); the active variant's SHA changes when the user
	// switches models, which naturally invalidates stale dismissals.
	DismissedRecommendations map[string]time.Time `json:"dismissed_recommendations,omitempty"`

	// LastBenchmark persists the most recent completed benchmark run
	// (waired#835 §12) so the declarative generation counter and its
	// result survive daemon restarts — the CP compares the pushed gen
	// against desired_benchmark_gen, and an in-memory-only record would
	// re-trigger a benchmark on every restart.
	LastBenchmark *BenchmarkRecord `json:"last_benchmark,omitempty"`

	// MeasuredVariants records what each variant actually decoded on
	// this host, so a model this machine has already measured as too
	// slow stops being the one it recommends to itself
	// (waired-agent#784).
	//
	// A MAP rather than a single latest record, because the step-down
	// walks more than one rung: a host that measured the 9B too slow,
	// stepped down and measured the 4B too slow as well must exclude
	// both. Keeping only the newest would re-recommend the 9B the
	// moment the 4B was measured, and the ladder would oscillate
	// instead of descending.
	//
	// Keyed by VariantSHA, the same key DismissedRecommendations uses
	// and for the same reason: the digest covers the variant's identity
	// and its source, so a re-quantized or re-tagged variant is a
	// different key and its old figure expires on its own rather than
	// through a rule someone has to remember.
	//
	// This is NOT the boot bench cache (~/.cache/waired/bench.json).
	// That one is an optimisation, it is written only on the boot path,
	// and it may be cleared at any time; a record that decides what this
	// host recommends has to survive that, and has to include the runs
	// the CLI and the control plane ask for.
	MeasuredVariants map[string]VariantMeasurement `json:"measured_variants,omitempty"`
}

// VariantMeasurement is one variant's measured decode rate on this
// host — a figure, never a verdict. The floor it is compared against
// belongs to the caller (router.CodingAgentSelectionFloorTokps by
// default, overridable per host through agent config), and different
// questions may well settle on different numbers, so what is stored
// here stays the raw measurement.
type VariantMeasurement struct {
	// ModelID and VariantID name what was measured. BenchmarkRecord
	// below deliberately names neither — its identity is the generation
	// the run was requested under, not its subject — which is why the
	// model identity BenchResult already carries was dropped at this
	// boundary and had to be picked back up (waired-agent#784).
	ModelID   string `json:"model_id,omitempty"`
	VariantID string `json:"variant_id,omitempty"`

	MeasuredTokps float64 `json:"measured_tokps,omitempty"`

	// Method is which of the measurement methods produced the figure,
	// for the reason BenchmarkRecord.Method is persisted: a wall_clock
	// number carries request overhead and must stay low-confidence
	// after a restart too (waired-agent#199).
	Method string `json:"method,omitempty"`

	// EngineKind and EngineVersion identify the engine that produced
	// the counters. A measurement is only comparable within an engine
	// build — waired#668 is the same lesson from the boot benchmark's
	// cache, where an Ollama bundle bump left it serving pre-bump
	// numbers.
	EngineKind    string `json:"engine_kind,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`

	MeasuredAt time.Time `json:"measured_at"`
}

// BenchmarkRecord is the persisted completion record of a benchmark
// job. Gen is the desired_benchmark_gen generation the run was
// requested under (0 for boot/CLI-triggered runs).
type BenchmarkRecord struct {
	Gen           int     `json:"gen,omitempty"`
	MeasuredTokps float64 `json:"measured_tokps,omitempty"`
	// ModelID and VariantID name WHAT the run measured. The record's
	// identity used to be the generation it was requested under and
	// nothing else, so a figure survived the model it described: the
	// setup wizard renders this number directly above the button that
	// changes the model, and nothing suppressed it when that button was
	// pressed (waired-agent#971).
	//
	// Empty on a record written before this field, and on a run that
	// measured nothing. An empty ModelID is UNLABELLED, not "no model" —
	// benchDescribes keeps such a figure rather than discarding it, so a
	// host that upgrades mid-flight behaves as it did before.
	ModelID   string `json:"model_id,omitempty"`
	VariantID string `json:"variant_id,omitempty"`
	// Method and SpreadPct describe HOW the figure was obtained: which
	// of the three measurement methods produced it, and how far apart
	// the samples behind it were. Persisted alongside the number because
	// a wall_clock result carries request overhead and must stay
	// low-confidence after a restart too — a bare tokens/s that has
	// forgotten its method reads as authoritative (waired-agent#199).
	Method    string  `json:"method,omitempty"`
	SpreadPct float64 `json:"spread_pct,omitempty"`
	Trials    int     `json:"trials,omitempty"`

	Failed bool   `json:"failed,omitempty"`
	Error  string `json:"error,omitempty"`
	// Outcome is WHICH ending the run had, not merely that it had a bad
	// one: "measured" / "failed" / "engine_not_ready" / "skipped". Failed
	// says a figure is unusable; Outcome says whether the host was ever
	// asked, and those are different claims about a machine
	// (waired-agent#203).
	//
	// Persisted because every consumer of a failure has to re-derive the
	// distinction otherwise, and the surface that most needs it — the
	// setup benchmark row — flattened every failure to an internal error
	// for want of it. Empty on a record written before this field, which
	// reads as "unknown ending" and keeps the old flattening.
	Outcome    string    `json:"outcome,omitempty"`
	MeasuredAt time.Time `json:"measured_at"`
}

// DismissalKey builds the map key for DismissedRecommendations from the
// active variant's content digest (catalog.VariantSHA) and the
// recommended target variant ID.
func DismissalKey(fromVariantSHA, toVariantID string) string {
	return fromVariantSHA + "→" + toVariantID
}

// ActiveSelection records the engine + model the agent serves on this
// host. Decided at install/refresh time, validated at every startup.
// Persisted across restarts; updated only by explicit user action
// (`waired runtimes refresh`, `waired models refresh`) — startup
// fallbacks are session-only and never write here.
type ActiveSelection struct {
	Runtime                  string           `json:"runtime"`
	RuntimeVersion           string           `json:"runtime_version,omitempty"`
	ModelID                  string           `json:"model_id"`
	VariantID                string           `json:"variant_id"`
	DecidedAt                time.Time        `json:"decided_at,omitempty"`
	DecidedBy                string           `json:"decided_by,omitempty"` // "auto" | "user" | "migration"
	DecisionReason           []string         `json:"decision_reason,omitempty"`
	DecisionHardwareSnapshot HardwareSnapshot `json:"decision_hardware_snapshot,omitempty"`
}

// HardwareSnapshot freezes the host attributes that drove the active
// decision so the next-startup validator can detect drift (e.g. GPU
// removed, RAM downsized) and emit a Refresh prompt.
type HardwareSnapshot struct {
	GPUModel       string `json:"gpu_model,omitempty"`
	GPUVRAMTotalMB int    `json:"gpu_vram_total_mb,omitempty"`
	RAMTotalGB     int    `json:"ram_total_gb,omitempty"`
}

// RuntimeInstall captures one installed engine: its pinned version
// and (for Python-based engines) its venv path so the agent can spawn
// the right interpreter without hard-coding paths.
type RuntimeInstall struct {
	Version     string    `json:"version"`
	VenvPath    string    `json:"venv_path,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitempty"`
}

// ExternalManifestRef is a placeholder for the Step 3+ CP /model-manifests
// fetch result so its eventual on-disk shape is reserved here. No code
// reads this in Step 2.
type ExternalManifestRef struct {
	URL         string    `json:"url,omitempty"`
	ETag        string    `json:"etag,omitempty"`
	LastFetchAt time.Time `json:"last_fetch_at,omitempty"`
	ManifestIDs []string  `json:"manifest_ids,omitempty"`
}

// ModelState describes one row of the model cache.
//
// HFRepo / LocalPath are v2 additions for HF-fetched (vLLM) variants:
// HFRepo mirrors the manifest's source.repo_id at pull time so
// post-pull lookups don't need to re-traverse the catalog, and
// LocalPath is the absolute on-disk directory the engine uses with
// `--model <path>`.
type ModelState struct {
	VariantID  string    `json:"variant_id"`
	OllamaTag  string    `json:"ollama_tag,omitempty"`
	HFRepo     string    `json:"hf_repo,omitempty"`
	LocalPath  string    `json:"local_path,omitempty"`
	State      string    `json:"state"`
	SizeBytes  int64     `json:"size_bytes,omitempty"`
	PulledAt   time.Time `json:"pulled_at,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// EndpointState describes one running engine endpoint owned by this
// agent. Phase A only ever has at most one entry (single Ollama);
// Step 2 keeps the same invariant (= one engine per host).
type EndpointState struct {
	Runtime   string    `json:"runtime"`
	ModelID   string    `json:"model_id"`
	VariantID string    `json:"variant_id"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
}

// Store provides serialised, atomic read/write access to state.json.
// Phase A relies on the single-process invariant — only waired-agent
// mutates state.json — so an in-process mutex is sufficient. Phase B
// may layer flock(2) on top once the CLI gains a write path.
type Store struct {
	path          string
	mu            sync.Mutex
	migrationOpts MigrationOpts
}

// NewStore returns a Store that reads from / writes to path. The
// containing directory is created on first Save.
func NewStore(path string) *Store { return &Store{path: path} }

// WithMigrationOpts swaps the MigrationOpts the Store passes to
// MigrateInPlace on every Load. Callers (agent bootstrap) use this
// to inject the live HardwareSnapshot so v1 → v2 migration can
// stamp the migrated ActiveSelection with the host's current GPU/RAM.
func (s *Store) WithMigrationOpts(opts MigrationOpts) *Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.migrationOpts = opts
	return s
}

// MigrationOpts returns a copy of the currently configured options.
// Used by the bootstrap to log what hardware snapshot was applied.
func (s *Store) MigrationOpts() MigrationOpts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.migrationOpts
}

// Load returns the current State, or a fresh empty State if the file
// doesn't exist yet.
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{
				Version:   StateVersion,
				Models:    map[string]ModelState{},
				Endpoints: map[string]EndpointState{},
				Runtimes:  map[string]RuntimeInstall{},
			}, nil
		}
		return State{}, fmt.Errorf("catalog: read %s: %w", s.path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("catalog: parse %s: %w", s.path, err)
	}
	// Migrate older schemas in-place. Migration is idempotent for
	// already-current versions and only mutates the in-memory copy;
	// callers (agent bootstrap) decide whether to persist the result.
	MigrateInPlace(&st, s.migrationOpts)
	return st, nil
}

// Save writes st to disk via temp-file + atomic rename. Permissions
// are 0600 because state.json may eventually carry tokens / paths the
// user wouldn't want world-readable.
func (s *Store) Save(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(st)
}

func (s *Store) saveLocked(st State) error {
	if st.Version == 0 {
		st.Version = StateVersion
	}
	if st.Models == nil {
		st.Models = map[string]ModelState{}
	}
	if st.Endpoints == nil {
		st.Endpoints = map[string]EndpointState{}
	}

	if err := secrets.SecureDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("catalog: mkdir for state: %w", err)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("catalog: marshal state: %w", err)
	}
	if err := secrets.WriteSecret(s.path, data); err != nil {
		return fmt.Errorf("catalog: write state: %w", err)
	}
	return nil
}

// Update is a load → mutate → save convenience that holds the store
// mutex across the whole operation, ensuring concurrent goroutines
// see each others' writes.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	fn(&st)
	return s.saveLocked(st)
}

// DefaultStatePath returns the on-disk location of state.json under
// <StateDir>/inference/, where <StateDir> is resolved by
// platform/paths (per-OS, $WAIRED_STATE_DIR overrides). Prior to the
// platform/paths consolidation this lived under $XDG_STATE_HOME on
// Linux — operators upgrading across that boundary need to migrate
// the file by hand (the agent will re-create a default state.json
// if the new path is missing).
func DefaultStatePath() string {
	return filepath.Join(paths.StateDir(paths.AutoDetect), "inference", "state.json")
}
