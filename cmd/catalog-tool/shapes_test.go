package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/gateway"
)

// shapeTestTag is a tag the bundled catalog actually ships, so
// resolveTag has somewhere to put the record.
const shapeTestTag = "gpt-oss:20b"

func fullShapeReport(t *testing.T) agentgrade.ShapeReport {
	t.Helper()
	shapes := gateway.EngineShapes()
	rep := agentgrade.ShapeReport{
		Model:         shapeTestTag,
		Engine:        "ollama",
		EngineVersion: "0.32.15",
		Expected:      len(shapes),
		Measured:      len(shapes),
		ControlOK:     true,
		AgentRevision: "abc123abc123",
		Started:       time.Now(),
	}
	for _, s := range shapes {
		rep.Results = append(rep.Results, agentgrade.ShapeResult{
			Shape:          s.Name,
			Digest:         s.Digest(),
			Outcome:        agentgrade.ShapeAccepted,
			Status:         200,
			SentRoles:      s.EngineRoles(),
			EngineSawRoles: s.EngineRoles(),
		})
	}
	return rep
}

func writeShapeReportFile(t *testing.T, rep agentgrade.ShapeReport) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.json")
	// The wrapped form: what the GPU lane's grade run emits.
	b, err := json.Marshal(struct {
		Shapes agentgrade.ShapeReport `json:"shapes"`
	}{Shapes: rep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func emptyStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requestshapes.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"models":{},"baseline":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func readStore(t *testing.T, path string) catalog.RequestShapeSet {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var set catalog.RequestShapeSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("decode store: %v", err)
	}
	return set
}

// TestShapesImportRoundTripsEveryField is the assertion agentgrade's
// importer never got: its record is built as a fresh struct literal, so
// a field added to the type is silently dropped on the next import and
// nothing notices. Supplying every field and asserting none came back
// zero is the only check that closes that permanently.
func TestShapesImportRoundTripsEveryField(t *testing.T) {
	store := emptyStore(t)
	report := writeShapeReportFile(t, fullShapeReport(t))

	err := runShapes([]string{
		"--import", report,
		"--store", store,
		"--host", "nvidia-24gb-discrete",
		"--run-url", "https://github.com/waired-ai/waired-agent/actions/runs/12345",
		"--retrieved", "2026-08-28",
		"--notes", "round-trip test",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	set := readStore(t, store)
	rec, ok := set.Lookup("gpt-oss-20b", "mxfp4-gguf")
	if !ok {
		t.Fatalf("no record was written: %+v", set.Models)
	}

	v := reflect.ValueOf(rec)
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("%s came back zero: the import dropped it", f.Name)
		}
	}

	if len(rec.Shapes) != len(gateway.EngineShapes()) {
		t.Errorf("recorded %d shapes, want %d", len(rec.Shapes), len(gateway.EngineShapes()))
	}
	for _, s := range gateway.EngineShapes() {
		got, ok := rec.Shapes[s.Name]
		if !ok {
			t.Errorf("shape %q was not recorded", s.Name)
			continue
		}
		if got.Digest != s.Digest() {
			t.Errorf("shape %q recorded at digest %s, want %s", s.Name, got.Digest, s.Digest())
		}
		if len(got.EngineSawRoles) == 0 {
			t.Errorf("shape %q recorded no engine_saw_roles", s.Name)
		}
	}
}

func TestShapesImportRefusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*agentgrade.ShapeReport)
		wantSub string
	}{
		{
			name:    "a run whose control did not hold",
			mutate:  func(r *agentgrade.ShapeReport) { r.ControlOK = false },
			wantSub: "negative control",
		},
		{
			name: "a run with an errored row",
			mutate: func(r *agentgrade.ShapeReport) {
				r.Results[0].Outcome = agentgrade.ShapeError
				r.Results[0].Detail = "connection refused"
			},
			wantSub: "could not be measured",
		},
		{
			name:    "a partial run",
			mutate:  func(r *agentgrade.ShapeReport) { r.Measured = r.Expected - 1 },
			wantSub: "partial run",
		},
		{
			name:    "no engine version",
			mutate:  func(r *agentgrade.ShapeReport) { r.EngineVersion = "" },
			wantSub: "engine version",
		},
		{
			name:    "a modified tree",
			mutate:  func(r *agentgrade.ShapeReport) { r.AgentRevision = "abc123abc123-dirty" },
			wantSub: "modified tree",
		},
		{
			name:    "an unstamped harness",
			mutate:  func(r *agentgrade.ShapeReport) { r.AgentRevision = "" },
			wantSub: "no agent revision",
		},
		{
			name: "a row measured at another digest",
			mutate: func(r *agentgrade.ShapeReport) {
				r.Results[0].Digest = "0000deadbeef"
			},
			wantSub: "the shipped table asks it at",
		},
		{
			name: "a report that skipped a row the table asks for",
			mutate: func(r *agentgrade.ShapeReport) {
				r.Results = r.Results[1:]
				r.Expected = len(r.Results)
				r.Measured = len(r.Results)
			},
			wantSub: "did not measure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := fullShapeReport(t)
			tc.mutate(&rep)
			store := emptyStore(t)
			err := runShapes([]string{
				"--import", writeShapeReportFile(t, rep),
				"--store", store,
				"--retrieved", "2026-08-28",
			})
			if err == nil {
				t.Fatal("import should have been refused")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantSub)
			}
			if set := readStore(t, store); countShapeRecords(set) != 0 {
				t.Error("a refused import still wrote to the store")
			}
		})
	}
}

func TestShapesImportRefusesAForeignRunURL(t *testing.T) {
	err := runShapes([]string{
		"--import", writeShapeReportFile(t, fullShapeReport(t)),
		"--store", emptyStore(t),
		"--retrieved", "2026-08-28",
		"--run-url", "https://example.com/runs/1",
	})
	if err == nil || !strings.Contains(err.Error(), "not an Actions run") {
		t.Fatalf("err = %v", err)
	}
}

func TestShapesImportRequiresRetrieved(t *testing.T) {
	err := runShapes([]string{
		"--import", writeShapeReportFile(t, fullShapeReport(t)),
		"--store", emptyStore(t),
	})
	if err == nil || !strings.Contains(err.Error(), "--retrieved") {
		t.Fatalf("err = %v", err)
	}
}

// TestShapesImportClearsTheBaselineEntry: measuring a variant retires
// its exemption. Leaving it would keep the gate looking past evidence
// it now has.
func TestShapesImportClearsTheBaselineEntry(t *testing.T) {
	store := filepath.Join(t.TempDir(), "requestshapes.json")
	seed := `{"schema":1,"models":{},"baseline":{"gpt-oss-20b/mxfp4-gguf":"whatever"}}`
	if err := os.WriteFile(store, []byte(seed), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := runShapes([]string{
		"--import", writeShapeReportFile(t, fullShapeReport(t)),
		"--store", store,
		"--retrieved", "2026-08-28",
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	set := readStore(t, store)
	if _, still := set.Baseline["gpt-oss-20b/mxfp4-gguf"]; still {
		t.Error("the baseline exemption outlived the measurement that retired it")
	}
}

func TestSeedBaselineIsOneShot(t *testing.T) {
	store := emptyStore(t)
	if err := runShapes([]string{"--seed-baseline", "--store", store}); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	set := readStore(t, store)
	if len(set.Baseline) == 0 {
		t.Fatal("seeding exempted nothing")
	}
	err := runShapes([]string{"--seed-baseline", "--store", store})
	if err == nil || !strings.Contains(err.Error(), "seeded once") {
		t.Fatalf("a second seed should be refused: %v", err)
	}
}

// TestSeededBaselineClosesEveryGap is what PR-2 arms: once the existing
// catalog is exempted, --check is quiet. Run here against a temporary
// store so the assertion holds before the shipped store is seeded.
func TestSeededBaselineClosesEveryGap(t *testing.T) {
	store := emptyStore(t)
	if err := runShapes([]string{"--seed-baseline", "--store", store}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := runShapes([]string{"--check", "--store", store}); err != nil {
		t.Fatalf("--check should be quiet against a seeded baseline: %v", err)
	}
}

// TestCheckFailsWithNoEvidenceAndNoBaseline is the gate doing its job:
// an unexempted variant with no record is a failure, which is what a
// newly added model looks like.
func TestCheckFailsWithNoEvidenceAndNoBaseline(t *testing.T) {
	err := runShapes([]string{"--check", "--store", emptyStore(t)})
	if err == nil || !strings.Contains(err.Error(), "no current request-shape evidence") {
		t.Fatalf("err = %v", err)
	}
}

// TestCurrentShapeRefsMatchTheShippedTable keeps the gate's idea of the
// table and the table itself from drifting.
func TestCurrentShapeRefsMatchTheShippedTable(t *testing.T) {
	refs := currentShapeRefs()
	shapes := gateway.EngineShapes()
	if len(refs) != len(shapes) {
		t.Fatalf("%d refs for %d shapes", len(refs), len(shapes))
	}
	if len(refs) == 0 {
		t.Fatal("no shapes: this guard would be checking nothing")
	}
	for i, r := range refs {
		if r.Name != shapes[i].Name || r.Digest != shapes[i].Digest() {
			t.Errorf("ref %d = %+v, want %s/%s", i, r, shapes[i].Name, shapes[i].Digest())
		}
	}
}
