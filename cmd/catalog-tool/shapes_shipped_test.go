package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// The shipped catalog, asserted against the shipped shape table.
//
// internal/catalog cannot make this assertion: internal/gateway already
// imports it, so reading the table from there would close a cycle. Its
// own gap tests therefore run against synthetic manifests and a
// wantShapes() fixture whose digests are the literals "aaa" and "bbb" —
// they prove the LOGIC and say nothing about what is shipped.
//
// cmd/catalog-tool imports both, so this is where the two can be put in
// front of each other. It runs in `go test ./...`, which means the
// required unit-tests job and a plain local run fail on a gap, not only
// the lint job's `shapes --check`.
func TestShippedCatalogHasNoShapeGaps(t *testing.T) {
	bundled, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	if len(bundled) == 0 {
		t.Fatal("no offered manifests — this guard is checking nothing")
	}
	set, err := catalog.RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	grades, err := catalog.AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}

	want := currentShapeRefs()
	if len(want) == 0 {
		t.Fatal("the shipped shape table is empty — every record would read as current")
	}

	for _, g := range set.RequestShapeGaps(bundled, grades.Unmeasurable, want) {
		name := g.ModelID
		if g.VariantID != "" {
			name += "/" + g.VariantID
		}
		t.Errorf("%s has no current request-shape evidence: %s\n"+
			"Measure it (make e2e-agentgrade MODEL=<tag> JSON=/tmp/r.json, then "+
			"catalog-tool shapes --import), or state the exemption.", name, g.Reason)
	}

	// The verdict, not just the presence. Coverage never reads an
	// outcome, so a variant measured REFUSING the shape Claude Code
	// sends is not a gap — see TestRejectedShapes in internal/catalog.
	for _, r := range set.RejectedShapes(bundled, grades.Unmeasurable) {
		t.Errorf("%s/%s: the engine refused %q (status %d, %s) on %s. A model that cannot "+
			"render what a coding agent sends is not one to offer (#1035).",
			r.ModelID, r.VariantID, r.Shape, r.Status, r.Marker, r.EngineVersion)
	}
}

// TestShippedRecordsMatchTheShippedTable: a record is evidence about the
// rows the table asks TODAY. The digest is what ties the two together,
// so a stored row naming a digest the table no longer asks is stale
// evidence wearing a current label.
func TestShippedRecordsMatchTheShippedTable(t *testing.T) {
	set, err := catalog.RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	byName := map[string]string{}
	for _, ref := range currentShapeRefs() {
		byName[ref.Name] = ref.Digest
	}

	checked := 0
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			for name, outcome := range rec.Shapes {
				want, ok := byName[name]
				if !ok {
					// A row the table dropped. Harmless, but it should
					// not read as evidence for anything.
					continue
				}
				checked++
				if outcome.Digest != want {
					t.Errorf("%s/%s: row %q recorded at digest %s, the shipped table asks it at %s",
						modelID, variantID, name, outcome.Digest, want)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no stored row matched a shipped row by name — this guard is checking nothing")
	}
}

// The engine version is part of the finding, not decoration: the same
// shape is refused by ollama 0.32.13, tolerated by 0.32.14 and merged by
// 0.32.15. A record that does not name a build says nothing.
func TestShippedRecordsNameAnEngineBuild(t *testing.T) {
	set, err := catalog.RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	seen := 0
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			seen++
			if strings.TrimSpace(rec.EngineVersion) == "" {
				t.Errorf("%s/%s carries no engine version", modelID, variantID)
			}
			if strings.TrimSpace(rec.Engine) == "" {
				t.Errorf("%s/%s carries no engine name", modelID, variantID)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no records in the store — this guard is checking nothing")
	}
}

// The two stores answer different questions about the same variants, and
// internal/catalog/requestshapes.go says why they are separate and then
// names the risk out loud: "bending one of the two to fit the other is
// how they would start disagreeing about which models were measured."
// Nothing checked any of it (waired-agent#1117).
//
// One direction is assertable and true today: a variant whose request
// shapes were measured must also carry a verdict. A shape matrix says
// the engine accepted the messages a coding agent sends; it says nothing
// about whether the model can then drive a tool call, which is the
// question the grade store answers. A shape record with no verdict is a
// variant measured for the easier of the two questions only.
//
// The other direction is deliberately NOT asserted. 18 verdicts predate
// the shape table entirely, which is exactly what the baseline exemption
// records — demanding shapes for every verdict would be red on arrival.
// Nor are host/agent_revision/retrieved compared: the shipped records
// come from runs months apart, so an equality check would either fail
// today or be narrowed until it asserted nothing.
func TestEveryShapeRecordHasAVerdict(t *testing.T) {
	shapes, err := catalog.RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	grades, err := catalog.AgentGrades()
	if err != nil {
		t.Fatalf("AgentGrades: %v", err)
	}

	seen := 0
	for modelID, m := range shapes.Models {
		for variantID := range m.Variants {
			seen++
			g, ok := grades.Models[modelID]
			if !ok {
				t.Errorf("%s/%s has request shapes and no agent-grade record at all — "+
					"the engine accepting the messages is not the same claim as the model "+
					"driving a tool call", modelID, variantID)
				continue
			}
			if _, ok := g.Variants[variantID]; !ok {
				t.Errorf("%s/%s has request shapes and no verdict for that variant "+
					"(the model has verdicts for other variants)", modelID, variantID)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no request-shape records — this guard is checking nothing")
	}
}
