package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// emptyAgentGradeStore is the escape hatch --store exists for. Until
// waired-agent#1117 the importer wrote to a relative package constant, so
// no test could reach the write path and this file's whole subject —
// what a successful import actually records — had no coverage at all.
func emptyAgentGradeStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentgrade.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"models":{},"unmeasurable":{}}`), 0o644); err != nil {
		t.Fatalf("write store: %v", err)
	}
	return path
}

func readAgentGradeStore(t *testing.T, path string) catalog.AgentGradeSet {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var set catalog.AgentGradeSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatalf("decode store: %v", err)
	}
	return set
}

// gradeReportWithMatrix is a report as `make e2e-agentgrade` writes one:
// a grade, and the shape matrix that rode along beside it carrying the
// engine build the run observed.
func gradeReportWithMatrix(t *testing.T, engineVersion string) map[string]any {
	t.Helper()
	rep := validReport(t)
	rep["flaky"] = []string{"tool_choice_required"}
	rep["results"] = []map[string]any{{
		"case":          "tool_call",
		"verdict":       "ok",
		"trials":        12,
		"failed_trials": 0,
		"verdicts":      map[string]int{"ok": 12},
	}}
	rep["shapes"] = map[string]any{
		"model":          "granite4:350m",
		"engine":         "ollama",
		"engine_version": engineVersion,
	}
	return rep
}

func importGrade(t *testing.T, rep map[string]any, extra ...string) error {
	t.Helper()
	args := append([]string{"--import", writeReport(t, rep)}, extra...)
	return runAgentGrade(args)
}

// TestImportAgentGradeRoundTripsEveryField is the assertion
// cmd/catalog-tool/shapes_test.go names as the one this importer never
// got. The record is built as a fresh struct literal, so a field added to
// VariantAgentGrade is silently dropped on the next import; supplying
// every field and asserting none came back zero is what closes that.
func TestImportAgentGradeRoundTripsEveryField(t *testing.T) {
	store := emptyAgentGradeStore(t)

	err := importGrade(t, gradeReportWithMatrix(t, "0.32.15"),
		"--store", store,
		"--host", "nvidia-24gb-discrete",
		"--run-url", "https://github.com/waired-ai/waired-agent/actions/runs/12345",
		"--retrieved", "2026-08-29",
		"--notes", "round-trip test",
	)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	set := readAgentGradeStore(t, store)
	m, ok := set.Models["granite4-350m"]
	if !ok {
		t.Fatalf("no model was written: %+v", set.Models)
	}
	rec, ok := m.Variants["bf16-gguf"]
	if !ok {
		t.Fatalf("no variant was written: %+v", m.Variants)
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
}

// The engine build is read off the runtime adapter by the run and rides
// the same artifact. It was a typed flag, and the only CI lane that
// produces reports did not pass it — so a lane-produced verdict landed
// with no engine build at all (waired-agent#1117).
func TestImportAgentGradeDerivesTheEngineBuild(t *testing.T) {
	store := emptyAgentGradeStore(t)

	err := importGrade(t, gradeReportWithMatrix(t, "0.32.15"),
		"--store", store,
		"--host", "nvidia-24gb-discrete",
		"--retrieved", "2026-08-29",
	)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	rec := readAgentGradeStore(t, store).Models["granite4-350m"].Variants["bf16-gguf"]
	if rec.EngineVersion != "0.32.15" {
		t.Errorf("engine_version = %q, want 0.32.15 derived from the report", rec.EngineVersion)
	}
}

func TestImportAgentGradeRefusals(t *testing.T) {
	valid := func() map[string]any { return gradeReportWithMatrix(t, "0.32.15") }

	cases := []struct {
		name string
		rep  map[string]any
		args []string
		want string
	}{
		{
			name: "a typed engine version that disagrees with the run",
			rep:  valid(),
			args: []string{"--engine-version", "0.32.13"},
			want: "the run observed",
		},
		{
			name: "no matrix and no flag: the build cannot be named",
			rep: func() map[string]any {
				r := valid()
				delete(r, "shapes")
				return r
			}(),
			want: "cannot name the engine",
		},
		{
			name: "a host that is not a declared hardware class",
			rep:  valid(),
			args: []string{"--host", "sv-mag"},
			want: "not a known hardware class",
		},
		{
			name: "no host at all",
			rep:  valid(),
			args: []string{"--host", ""},
			want: "--host is required",
		},
		{
			// The emptiness check both importers shipped with accepted
			// this: len() is not a date.
			name: "a date that is not a date",
			rep:  valid(),
			args: []string{"--retrieved", "2026-8-030"},
			want: "is not a date",
		},
		{
			name: "no date",
			rep:  valid(),
			args: []string{"--retrieved", ""},
			want: "--retrieved YYYY-MM-DD is required",
		},
		{
			name: "a run URL that is not an Actions run for this repository",
			rep:  valid(),
			args: []string{"--run-url", "https://example.com/runs/1"},
			want: "not an Actions run",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := emptyAgentGradeStore(t)
			args := []string{
				"--store", store,
				"--host", "nvidia-24gb-discrete",
				"--retrieved", "2026-08-29",
			}
			args = append(args, tc.args...)
			err := importGrade(t, tc.rep, args...)
			if err == nil {
				t.Fatalf("import succeeded; want a refusal mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			// A refusal must not have written anything: a half-imported
			// store is worse than no import.
			if set := readAgentGradeStore(t, store); len(set.Models) != 0 {
				t.Errorf("a refused import wrote %d model(s)", len(set.Models))
			}
		})
	}
}

// Two runs on different engine builds are not two samples of one thing:
// the same request shape is refused by one ollama build and merged by the
// next. pool() already refuses to mix fixture and harness revisions for
// the same reason.
func TestImportAgentGradeRefusesPoolingAcrossEngineBuilds(t *testing.T) {
	store := emptyAgentGradeStore(t)
	a := writeReport(t, gradeReportWithMatrix(t, "0.32.13"))
	b := writeReport(t, gradeReportWithMatrix(t, "0.32.15"))

	err := runAgentGrade([]string{
		"--import", a, "--import", b,
		"--store", store,
		"--host", "nvidia-24gb-discrete",
		"--retrieved", "2026-08-29",
	})
	if err == nil {
		t.Fatal("pooling across engine builds was accepted")
	}
	if !strings.Contains(err.Error(), "different engine") {
		t.Errorf("error = %v, want it to name the engine build", err)
	}
}

// The vocabulary is only useful if it is not empty and the shipped stores
// live inside it — a list nobody uses would pass every check above while
// asserting nothing.
func TestHostClassVocabularyIsUsed(t *testing.T) {
	if len(catalog.HostClasses) == 0 {
		t.Fatal("no host classes declared — the guard above is checking nothing")
	}
	grades, err := catalog.AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}
	used := 0
	for _, m := range grades.Models {
		for _, rec := range m.Variants {
			if catalog.ValidHostClass(rec.Host) {
				used++
			}
		}
	}
	if used == 0 {
		t.Fatal("no stored verdict names a declared host class — the vocabulary and the " +
			"store have already drifted apart")
	}
}
