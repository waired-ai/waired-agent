package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

func init() {
	subcommands["shapes"] = subcommand{
		run:     runShapes,
		summary: "request-shape matrix: report coverage, or fold in a measurement (#1095)",
	}
}

// shapesPath is where the store lives. Passed around rather than read
// from a package constant everywhere, so the write path is reachable
// from a test — agentgrade's importer writes to a relative constant and
// as a result has no successful-import test at all, only refusals.
const shapesPath = "internal/catalog/requestshapes.json"

func runShapes(args []string) error {
	fs := flag.NewFlagSet("shapes", flag.ContinueOnError)
	check := fs.Bool("check", false, "fail when an offered variant has neither a current record nor an exemption")
	requireAccepted := fs.Bool("require-accepted", false,
		"fail when an offered variant's record says the engine refused a shape")
	var importPaths repeatedPath
	fs.Var(&importPaths, "import", "fold a probe report into the store (repeatable)")
	host := fs.String("host", "", "hardware class the measurement ran on (never an identifier)")
	runURL := fs.String("run-url", "", "CI run the measurement came from, when it came from one")
	retrieved := fs.String("retrieved", "", "measurement date, YYYY-MM-DD (required with --import)")
	notes := fs.String("notes", "", "free-text note stored with the record")
	seedBaseline := fs.Bool("seed-baseline", false,
		"one-shot: exempt every offered variant that has no record yet")
	storePath := fs.String("store", shapesPath, "path to the store (for tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The OFFERED set drives coverage: an internal-only model is not a
	// model users are handed, so nothing demands evidence for it.
	bundled, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("shapes: load bundled catalog: %w", err)
	}
	// Import resolves against the COMPLETE set, for the reason
	// agentgrade already records as a shipped bug (internal/catalog/
	// internal_resolve_test.go): resolving a measurement against the
	// offered set refuses the measurement of a model that is shipped but
	// withheld. granite4:350m is exactly that shape — it is the default
	// of `make e2e-agentgrade`, the tag the routing sentinel pins, and
	// the model the GPU lane's own instructions name, and its report
	// could not be imported at all.
	all, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		return fmt.Errorf("shapes: load complete catalog: %w", err)
	}

	if len(importPaths) > 0 {
		// Shared with the agent-grade importer: the two stores must not
		// disagree about what an operator may claim (waired-agent#1117).
		for _, err := range []error{
			checkRetrieved("shapes", *retrieved),
			checkHostClass("shapes", *host),
			checkRunURL("shapes", *runURL),
		} {
			if err != nil {
				return err
			}
		}
		return importShapes(importPaths, shapeImportOpts{
			Host:      *host,
			RunURL:    *runURL,
			Retrieved: *retrieved,
			Notes:     *notes,
			Bundled:   all,
			StorePath: *storePath,
		})
	}

	set, err := loadShapes(*storePath)
	if err != nil {
		return err
	}

	grades, err := catalog.AgentGrades()
	if err != nil {
		return fmt.Errorf("shapes: load agent grades: %w", err)
	}

	if *seedBaseline {
		return seedShapeBaseline(set, bundled, grades.Unmeasurable, *storePath)
	}

	want := currentShapeRefs()
	gaps := set.RequestShapeGaps(bundled, grades.Unmeasurable, want)
	rejected := set.RejectedShapes(bundled, grades.Unmeasurable)
	printShapeReport(os.Stdout, set, gaps, rejected, want)

	if *check && len(gaps) > 0 {
		return fmt.Errorf("shapes: %d variant(s) have no current request-shape evidence", len(gaps))
	}
	if *requireAccepted && len(rejected) > 0 {
		return fmt.Errorf("shapes: %d shape(s) are refused by an offered variant's engine — "+
			"a model that cannot render what a coding agent sends is not one to offer", len(rejected))
	}
	return nil
}

// currentShapeRefs names the shipped shape table. internal/catalog
// cannot read it (internal/gateway already imports internal/catalog, so
// the other direction would close a cycle), so the caller that has both
// supplies it — the same shape as CoverageGaps taking a fixture
// revision it cannot compute.
func currentShapeRefs() []catalog.ShapeRef {
	shapes := gateway.EngineShapes()
	out := make([]catalog.ShapeRef, 0, len(shapes))
	for _, s := range shapes {
		out = append(out, catalog.ShapeRef{Name: s.Name, Digest: s.Digest()})
	}
	return out
}

func printShapeReport(w io.Writer, set catalog.RequestShapeSet, gaps []catalog.RequestShapeGap,
	rejected []catalog.RequestShapeRejection, want []catalog.ShapeRef) {
	reportf(w, "request-shape matrix: %d shapes asked, %d variant(s) recorded\n", len(want), countShapeRecords(set))

	if drift := set.StaleEngineVersions(runtime.OllamaPinnedVersion); len(drift) > 0 {
		// Reported, never a gap. A pin bump does not re-open the
		// question for a model already measured; being silent about the
		// drift is what would make the bump look free.
		reportf(w, "\n%d record(s) measured on an engine other than the pin (%s):\n",
			len(drift), runtime.OllamaPinnedVersion)
		for _, d := range drift {
			reportf(w, "  %s/%s — %s\n", d.ModelID, d.VariantID, d.Reason)
		}
	}

	if n := len(set.Baseline); n > 0 {
		reportf(w, "\n%d variant(s) exempt as baseline (in the catalog before this check existed)\n", n)
	}

	// Printed whether or not --require-accepted is set. A refusal is the
	// finding this table was built to surface, and a run that saw one
	// must not be able to look like a run that saw none.
	if len(rejected) > 0 {
		reportf(w, "\n%d shape(s) refused by an offered variant's engine:\n", len(rejected))
		for _, r := range rejected {
			reportf(w, "  %s/%s — %s refused (status %d, %s) on engine %s\n",
				r.ModelID, r.VariantID, r.Shape, r.Status, r.Marker, r.EngineVersion)
		}
	}

	if len(gaps) == 0 {
		reportf(w, "\nno gaps\n")
		return
	}
	reportf(w, "\n%d variant(s) with no current evidence:\n", len(gaps))
	for _, g := range gaps {
		if g.VariantID == "" {
			reportf(w, "  %s — %s\n", g.ModelID, g.Reason)
			continue
		}
		reportf(w, "  %s/%s — %s\n", g.ModelID, g.VariantID, g.Reason)
	}
}

func countShapeRecords(set catalog.RequestShapeSet) int {
	n := 0
	for _, m := range set.Models {
		n += len(m.Variants)
	}
	return n
}

type shapeImportOpts struct {
	Host      string
	RunURL    string
	Retrieved string
	Notes     string
	Bundled   []catalog.Manifest
	StorePath string
}

// shapeReportFile is what --import reads: either a probe report with a
// `shapes` object (what the GPU lane's agentgrade run emits) or a bare
// matrix.
type shapeReportFile struct {
	Shapes *agentgrade.ShapeReport `json:"shapes"`
}

func importShapes(paths []string, o shapeImportOpts) error {
	set, err := loadShapes(o.StorePath)
	if err != nil {
		return err
	}
	want := currentShapeRefs()

	for _, path := range paths {
		rep, err := readShapeReport(path)
		if err != nil {
			return err
		}
		if err := checkShapeReport(*rep, path, want); err != nil {
			return err
		}
		modelID, variantID, err := resolveTag(rep.Model, o.Bundled)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		sha, err := variantSHAFor(o.Bundled, modelID, variantID)
		if err != nil {
			return err
		}

		rec := catalog.VariantRequestShapes{
			VariantSHA:    sha,
			Engine:        rep.Engine,
			EngineVersion: rep.EngineVersion,
			AgentRevision: rep.AgentRevision,
			Host:          o.Host,
			RunURL:        o.RunURL,
			Retrieved:     o.Retrieved,
			Notes:         o.Notes,
			Shapes:        map[string]catalog.ShapeOutcome{},
		}
		for _, r := range rep.Results {
			rec.Shapes[r.Shape] = catalog.ShapeOutcome{
				Digest:         r.Digest,
				Outcome:        string(r.Outcome),
				Status:         r.Status,
				Marker:         r.Marker,
				EngineSawRoles: r.EngineSawRoles,
			}
		}

		if set.Models == nil {
			set.Models = map[string]catalog.ModelRequestShapes{}
		}
		entry, ok := set.Models[modelID]
		if !ok || entry.Variants == nil {
			entry = catalog.ModelRequestShapes{Variants: map[string]catalog.VariantRequestShapes{}}
		}
		entry.Variants[variantID] = rec
		set.Models[modelID] = entry

		// A measured variant is no longer a baseline exemption. Leaving
		// it would keep the gate looking past a record it now has.
		delete(set.Baseline, catalog.BaselineKey(modelID, variantID))

		fmt.Printf("shapes: %s/%s recorded on %s %s (%d rejected of %d)\n",
			modelID, variantID, rep.Engine, rep.EngineVersion, len(rep.Rejected()), len(rep.Results))
	}

	return writeShapes(set, o.StorePath)
}

func readShapeReport(path string) (*agentgrade.ShapeReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("shapes: read %s: %w", path, err)
	}
	var wrapped shapeReportFile
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Shapes != nil {
		return wrapped.Shapes, nil
	}
	var bare agentgrade.ShapeReport
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, fmt.Errorf("shapes: decode %s: %w", path, err)
	}
	if bare.Model == "" {
		return nil, fmt.Errorf("shapes: %s carries no shape matrix — a grade run that did not drive one "+
			"says so by absence, and there is nothing here to record", path)
	}
	return &bare, nil
}

// checkShapeReport refuses everything a report has to get right before
// it can become a record. Mirrors the agent-grade importer's refusals:
// what it cannot prove is that a GPU produced the numbers, so what it
// does instead is leave a fabricated record nowhere to hide.
func checkShapeReport(rep agentgrade.ShapeReport, path string, want []catalog.ShapeRef) error {
	if err := rep.Valid(); err != nil {
		return fmt.Errorf("shapes: %s: %w", path, err)
	}
	if rep.AgentRevision == "" {
		return fmt.Errorf("shapes: %s carries no agent revision — a bare `go test` produces none; "+
			"measure with `make e2e-agentgrade`, which stamps it", path)
	}
	// strings.HasSuffix, the spelling the agent-grade importer uses. The
	// open-coded length test this replaces let a bare "-dirty" through:
	// len("-dirty") is 6, so the `> 6` guard excluded the one revision
	// string that is nothing but the marker.
	if strings.HasSuffix(rep.AgentRevision, "-dirty") {
		return fmt.Errorf("shapes: %s was measured from a modified tree (%s): the record would name "+
			"a commit that does not describe what ran", path, rep.AgentRevision)
	}
	if rep.Engine == "" {
		return fmt.Errorf("shapes: %s carries no engine name", path)
	}
	// Every shape the shipped table asks for has to be in the report,
	// at the digest the table asks it at. A report of a shorter table is
	// not evidence for this one.
	byName := map[string]agentgrade.ShapeResult{}
	for _, r := range rep.Results {
		byName[r.Shape] = r
	}
	for _, w := range want {
		got, ok := byName[w.Name]
		if !ok {
			return fmt.Errorf("shapes: %s did not measure %q", path, w.Name)
		}
		if got.Digest != w.Digest {
			return fmt.Errorf("shapes: %s measured %q at digest %s, the shipped table asks it at %s",
				path, w.Name, got.Digest, w.Digest)
		}
	}
	return nil
}

func variantSHAFor(manifests []catalog.Manifest, modelID, variantID string) (string, error) {
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return catalog.VariantSHA(v), nil
			}
		}
	}
	return "", fmt.Errorf("shapes: %s/%s is not in the bundled catalog", modelID, variantID)
}

// seedShapeBaseline exempts every offered variant that has no record,
// once. It refuses to run against a non-empty baseline: "regenerate the
// baseline" must never become the standing fix for a red gate.
func seedShapeBaseline(set catalog.RequestShapeSet, bundled []catalog.Manifest,
	unmeasurable map[string]string, storePath string) error {
	if len(set.Baseline) > 0 {
		return fmt.Errorf("shapes: the baseline already has %d entries — it is seeded once and "+
			"shrinks thereafter; measure the variant instead", len(set.Baseline))
	}
	set.Baseline = map[string]string{}
	var keys []string
	for _, m := range bundled {
		// A model no runner can host is already excused by the
		// agent-grade store's map, which RequestShapeGaps consults. A
		// baseline entry on top of it would be a second, permanent
		// exemption for a variant nobody was going to ask about — and
		// since seeding runs once, undoing it is a hand edit.
		if _, ok := unmeasurable[m.ModelID]; ok {
			continue
		}
		for _, v := range m.Variants {
			if !catalog.VariantServesOllama(v) {
				continue
			}
			if _, ok := set.Lookup(m.ModelID, v.VariantID); ok {
				continue
			}
			key := catalog.BaselineKey(m.ModelID, v.VariantID)
			set.Baseline[key] = catalog.VariantSHA(v)
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Println("baseline:", k)
	}
	fmt.Printf("shapes: %d variant(s) exempted; add these to baselineRatchet in "+
		"internal/catalog/requestshapes_test.go\n", len(keys))
	return writeShapes(set, storePath)
}

func loadShapes(path string) (catalog.RequestShapeSet, error) {
	if path == shapesPath {
		set, err := catalog.RequestShapes()
		if err != nil {
			return catalog.RequestShapeSet{}, fmt.Errorf("shapes: %w", err)
		}
		return set, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return catalog.RequestShapeSet{}, fmt.Errorf("shapes: read %s: %w", path, err)
	}
	var set catalog.RequestShapeSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return catalog.RequestShapeSet{}, fmt.Errorf("shapes: decode %s: %w", path, err)
	}
	return set, nil
}

func writeShapes(set catalog.RequestShapeSet, path string) error {
	if set.Schema == 0 {
		set.Schema = 1
	}
	b, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return fmt.Errorf("shapes: encode store: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("shapes: write %s: %w", path, err)
	}
	return nil
}
