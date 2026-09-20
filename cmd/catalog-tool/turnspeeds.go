package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

func init() {
	subcommands["turnspeeds"] = subcommand{
		run:     runTurnSpeeds,
		summary: "reference-host seconds per request: report coverage, or fold in state.json snapshots (#1400)",
	}
}

const turnSpeedsPath = "internal/catalog/turnspeeds.json"

// minTurnSpeedSamples is how many measurements of one build a record needs.
// The owner asked for repeated runs (2026-09-16); one run cannot show the
// spread the 5% step factor is compared against.
const minTurnSpeedSamples = 3

func runTurnSpeeds(args []string) error {
	fs := flag.NewFlagSet("turnspeeds", flag.ContinueOnError)
	check := fs.Bool("check", false, "fail when an offered ollama variant has no current measured record")
	var importPaths repeatedPath
	fs.Var(&importPaths, "import", "a state.json snapshot taken after a measurement (repeatable)")
	host := fs.String("host", "", "hardware class the measurements ran on (never an identifier)")
	backend := fs.String("backend", "", "what the engine ran on, e.g. vulkan")
	agentRevision := fs.String("agent-revision", "", "the agent build that measured")
	retrieved := fs.String("retrieved", "", "measurement date, YYYY-MM-DD (required with --import)")
	notes := fs.String("notes", "", "free-text note stored with each imported record")
	storePath := fs.String("store", turnSpeedsPath, "path to the store (for tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bundled, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("turnspeeds: load bundled catalog: %w", err)
	}
	if len(importPaths) > 0 {
		for _, err := range []error{
			checkRetrieved("turnspeeds", *retrieved),
			checkHostClass("turnspeeds", *host),
		} {
			if err != nil {
				return err
			}
		}
		return importTurnSpeeds(importPaths, turnSpeedImportOpts{
			Host: *host, Backend: *backend, AgentRevision: *agentRevision,
			Retrieved: *retrieved, Notes: *notes, Bundled: bundled, StorePath: *storePath,
		})
	}
	set, err := loadTurnSpeeds(*storePath)
	if err != nil {
		return err
	}
	gaps := catalog.TurnSpeedGaps(set, bundled)
	fmt.Printf("turnspeeds: host class %s, %d model(s) recorded\n", set.HostClass, len(set.Models))
	for _, g := range gaps {
		fmt.Printf("  no current measurement: %s\n", g)
	}
	if *check && len(gaps) > 0 {
		return fmt.Errorf("turnspeeds: %d offered ollama variant(s) have no current measurement", len(gaps))
	}
	return nil
}

type turnSpeedImportOpts struct {
	Host, Backend, AgentRevision, Retrieved, Notes string
	Bundled                                        []catalog.Manifest
	StorePath                                      string
}

// snapshotState is the part of state.json the importer reads.
type snapshotState struct {
	MeasuredVariants map[string]catalog.VariantMeasurement `json:"measured_variants"`
	LastBenchmark    *catalog.BenchmarkRecord              `json:"last_benchmark"`
}

func loadTurnSpeeds(path string) (catalog.TurnSpeedSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return catalog.TurnSpeedSet{}, fmt.Errorf("turnspeeds: read %s: %w", path, err)
	}
	var s catalog.TurnSpeedSet
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return catalog.TurnSpeedSet{}, fmt.Errorf("turnspeeds: decode %s: %w", path, err)
	}
	return s, nil
}

// importTurnSpeeds folds product measurements into the store. A snapshot
// contributes one measurement: the measured_variants entry its own
// last_benchmark wrote, and none when that run answered with a stored
// figure (cached) instead of measuring, or failed. The other entries a snapshot
// carries are older runs — other builds, other sessions, other engine
// flags — and the runner file beside it does not describe them. The same
// measurement seen in two snapshots (same measured_at) counts once. A
// variant gets a record only with at least minTurnSpeedSamples
// measurements taken at the product's own depth and window — anything
// else is not the figure the step-down compares.
func importTurnSpeeds(paths []string, o turnSpeedImportOpts) error {
	set, err := loadTurnSpeeds(o.StorePath)
	if err != nil {
		return err
	}
	if set.HostClass != "" && set.HostClass != o.Host {
		return fmt.Errorf("turnspeeds: the store holds %s measurements; --host %s would mix host classes",
			set.HostClass, o.Host)
	}
	set.HostClass = o.Host

	type key struct{ model, variant string }
	samples := map[key]map[time.Time]catalog.VariantMeasurement{}
	// flags holds the engine's launch flags per sample, read from
	// <name>.runner.txt beside <name>.state.json. Every sample needs one.
	flags := map[key]map[time.Time]string{}
	var notMeasured []string
	for _, p := range paths {
		runnerFlags := ""
		if b, err := os.ReadFile(strings.TrimSuffix(p, ".state.json") + ".runner.txt"); err == nil {
			runnerFlags = strings.TrimSpace(string(b))
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("turnspeeds: read %s: %w", p, err)
		}
		var st snapshotState
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("turnspeeds: decode %s: %w", p, err)
		}
		sha, m, ok := measuredByThisRun(st)
		if !ok {
			notMeasured = append(notMeasured, filepath.Base(p))
			continue
		}
		v, ok := findShippedVariant(o.Bundled, m.ModelID, m.VariantID)
		if !ok || catalog.VariantSHA(v) != sha {
			continue // not a shipped build, or not this build of it
		}
		if m.EngineKind != catalog.RuntimeOllama || m.EngineVersion == "" || m.TurnSeconds <= 0 ||
			m.DepthTokens < hostfit.SpeedMeasurementDepthTokens || m.AppliedWindow < hostfit.ServingWindow200k {
			continue
		}
		k := key{m.ModelID, m.VariantID}
		if samples[k] == nil {
			samples[k] = map[time.Time]catalog.VariantMeasurement{}
		}
		samples[k][m.MeasuredAt] = m
		if runnerFlags != "" {
			if flags[k] == nil {
				flags[k] = map[time.Time]string{}
			}
			flags[k][m.MeasuredAt] = runnerFlags
		}
	}
	if len(notMeasured) > 0 {
		fmt.Printf("turnspeeds: %d snapshot(s) hold no measurement of their own (the run reused a stored figure or failed): %s\n",
			len(notMeasured), strings.Join(notMeasured, ", "))
	}

	if set.Models == nil {
		set.Models = map[string]catalog.ModelTurnSpeeds{}
	}
	var imported, short []string
	for k, byTime := range samples {
		if len(byTime) < minTurnSpeedSamples {
			short = append(short, fmt.Sprintf("%s/%s (%d)", k.model, k.variant, len(byTime)))
			continue
		}
		// Ordered by when they were measured, because the samples come
		// out of a map: ms[0] decides fields below, and a range order Go
		// randomises would write a different record from the same
		// snapshots (waired-ai/waired-agent#1400 review).
		ms := make([]catalog.VariantMeasurement, 0, len(byTime))
		for _, m := range byTime {
			ms = append(ms, m)
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].MeasuredAt.Before(ms[j].MeasuredAt) })
		engineFlags := ""
		byTimeFlags := flags[k]
		if len(byTimeFlags) != len(byTime) {
			return fmt.Errorf("turnspeeds: %s/%s: %d of %d samples carry the engine's flags; save them beside each snapshot as <name>.runner.txt",
				k.model, k.variant, len(byTimeFlags), len(byTime))
		}
		for _, f := range byTimeFlags {
			if engineFlags == "" {
				engineFlags = f
			} else if f != engineFlags {
				return fmt.Errorf("turnspeeds: %s/%s: samples ran under different engine flags (%q vs %q); the batch ollama picks moves prefill, so they are not one figure",
					k.model, k.variant, engineFlags, f)
			}
		}
		engineVersion := ms[0].EngineVersion
		for _, m := range ms[1:] {
			if m.EngineVersion != engineVersion || m.AppliedWindow != ms[0].AppliedWindow || m.KVCacheType != ms[0].KVCacheType {
				return fmt.Errorf("turnspeeds: %s/%s: samples disagree on engine version, window or KV cache type", k.model, k.variant)
			}
			if m.NumParallel != ms[0].NumParallel {
				return fmt.Errorf("turnspeeds: %s/%s: samples ran with different num_parallel (%d vs %d); the slots the engine serves at once move prefill, so they are not one figure",
					k.model, k.variant, ms[0].NumParallel, m.NumParallel)
			}
		}
		// The depth each sample reached is read back from the engine and
		// need not be identical across runs; the filter above only asks
		// for at least SpeedMeasurementDepthTokens. Record the shallowest,
		// which is the depth every sample in the record reached.
		depth := ms[0].DepthTokens
		for _, m := range ms[1:] {
			depth = min(depth, m.DepthTokens)
		}
		turn := median(func(m catalog.VariantMeasurement) float64 { return m.TurnSeconds }, ms)
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, m := range ms {
			lo, hi = math.Min(lo, m.TurnSeconds), math.Max(hi, m.TurnSeconds)
		}
		v, _ := findShippedVariant(o.Bundled, k.model, k.variant)
		mt := set.Models[k.model]
		if mt.Variants == nil {
			mt.Variants = map[string]catalog.VariantTurnSpeed{}
		}
		mt.Variants[k.variant] = catalog.VariantTurnSpeed{
			VariantSHA:    catalog.VariantSHA(v),
			SourceDigest:  v.Source.Digest,
			Method:        catalog.TurnSpeedMeasured,
			Engine:        catalog.RuntimeOllama,
			EngineVersion: engineVersion,
			Backend:       o.Backend,
			TurnSeconds:   round2(turn),
			PrefillTokps:  round2(median(func(m catalog.VariantMeasurement) float64 { return m.PrefillTokps }, ms)),
			DecodeTokps:   round2(median(func(m catalog.VariantMeasurement) float64 { return m.MeasuredTokps }, ms)),
			SpreadPct:     round2((hi - lo) / turn * 100),
			Samples:       len(ms),
			DepthTokens:   depth,
			AppliedWindow: ms[0].AppliedWindow,
			KVCacheType:   ms[0].KVCacheType,
			NumParallel:   ms[0].NumParallel,
			EngineFlags:   engineFlags,
			AgentRevision: o.AgentRevision,
			Retrieved:     o.Retrieved,
			Notes:         o.Notes,
		}
		set.Models[k.model] = mt
		imported = append(imported, fmt.Sprintf("%s/%s %.1f s", k.model, k.variant, turn))
	}
	sort.Strings(imported)
	sort.Strings(short)
	out, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.StorePath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("turnspeeds: write %s: %w", o.StorePath, err)
	}
	for _, s := range imported {
		fmt.Printf("turnspeeds: imported %s\n", s)
	}
	for _, s := range short {
		fmt.Printf("turnspeeds: skipped %s — fewer than %d measurements\n", s, minTurnSpeedSamples)
	}
	return nil
}

func findShippedVariant(ms []catalog.Manifest, modelID, variantID string) (catalog.Variant, bool) {
	for _, m := range ms {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return v, true
			}
		}
	}
	return catalog.Variant{}, false
}

func median(f func(catalog.VariantMeasurement) float64, ms []catalog.VariantMeasurement) float64 {
	xs := make([]float64, 0, len(ms))
	for _, m := range ms {
		xs = append(xs, f(m))
	}
	sort.Float64s(xs)
	n := len(xs)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// measuredByThisRun is the measured_variants entry the snapshot's own
// last_benchmark wrote: same model and variant, and the same seconds,
// which both copy from one run. Not the same measured_at: the product
// stamps the two with separate clock reads (inference_recommendation.go).
// A run that answered with a stored figure (cached) or failed wrote no
// entry, and the one its variant holds is an earlier run's.
func measuredByThisRun(st snapshotState) (string, catalog.VariantMeasurement, bool) {
	lb := st.LastBenchmark
	if lb == nil || lb.Cached {
		return "", catalog.VariantMeasurement{}, false
	}
	for sha, m := range st.MeasuredVariants {
		if m.ModelID == lb.ModelID && m.VariantID == lb.VariantID && m.TurnSeconds == lb.TurnSeconds {
			return sha, m, true
		}
	}
	return "", catalog.VariantMeasurement{}, false
}
