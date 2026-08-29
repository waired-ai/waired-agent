package catalog

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

func TestRequestShapesDecodes(t *testing.T) {
	set, err := RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	if set.Schema != 1 {
		t.Errorf("schema = %d, want 1", set.Schema)
	}
	if set.Notes == "" {
		t.Error("the store should say what a record means and who writes it")
	}
}

// TestRequestShapeJSONHasNoUnknownFields keeps the file and the struct
// from drifting in either direction: a key with no field is dropped on
// the next write, and a field with no key is a promise nothing keeps.
func TestRequestShapeJSONHasNoUnknownFields(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(requestShapeJSON))
	dec.DisallowUnknownFields()
	var set RequestShapeSet
	if err := dec.Decode(&set); err != nil {
		t.Fatalf("requestshapes.json carries a field the struct does not declare: %v", err)
	}
}

var retrievedDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func TestRequestShapeEntriesCarryProvenance(t *testing.T) {
	set, err := RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	// Every rule below is enforced by a loop over set.Models. While that
	// map was empty the loop bodies were dead code and this file was the
	// one store walk in the tree with no such assertion — the same
	// mistake agentgrade_test.go names out loud ("this guard is checking
	// nothing"). A store with no record at all is not a well-formed
	// store; it is an unarmed gate.
	if countRecords(set) == 0 {
		t.Fatal("no request-shape records in the store — this guard is checking nothing")
	}
	for modelID, m := range set.Models {
		for variantID, rec := range m.Variants {
			where := modelID + "/" + variantID
			if rec.VariantSHA == "" {
				t.Errorf("%s: no variant_sha — nothing pins which variant was measured", where)
			}
			if rec.Engine == "" {
				t.Errorf("%s: no engine", where)
			}
			if rec.EngineVersion == "" {
				t.Errorf("%s: no engine_version — the same shape answers differently on different builds", where)
			}
			if rec.AgentRevision == "" {
				t.Errorf("%s: no agent_revision", where)
			}
			if !retrievedDate.MatchString(rec.Retrieved) {
				t.Errorf("%s: retrieved = %q, want YYYY-MM-DD", where, rec.Retrieved)
			}
			// One vocabulary for both stores: two spellings is how they
			// would start disagreeing about where a model was measured.
			if !ValidHostClass(rec.Host) {
				t.Errorf("%s: host = %q is not a declared hardware class (one of: %s)",
					where, rec.Host, HostClassList())
			}
			if len(rec.Shapes) == 0 {
				t.Errorf("%s: no shapes recorded", where)
			}
			for name, out := range rec.Shapes {
				if out.Digest == "" {
					t.Errorf("%s/%s: no digest — nothing says which row this answers", where, name)
				}
				if out.Outcome != ShapeAccepted && out.Outcome != ShapeRejected {
					t.Errorf("%s/%s: outcome = %q, want %q or %q", where, name, out.Outcome, ShapeAccepted, ShapeRejected)
				}
				if out.Status == 0 {
					t.Errorf("%s/%s: no status", where, name)
				}
				if len(out.EngineSawRoles) == 0 {
					t.Errorf("%s/%s: no engine_saw_roles", where, name)
				}
			}
		}
	}
}

// TestRequestShapeKeysExistInCatalog keeps a record from outliving the
// variant it describes.
func TestRequestShapeKeysExistInCatalog(t *testing.T) {
	set, err := RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	if countRecords(set) == 0 {
		t.Fatal("no request-shape records in the store — this guard is checking nothing")
	}
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for modelID, m := range set.Models {
		for variantID := range m.Variants {
			if !variantExists(all, modelID, variantID) {
				if _, retired := protocatalog.LookupRetirement(modelID); retired {
					continue
				}
				t.Errorf("record for %s/%s names no shipped variant", modelID, variantID)
			}
		}
	}
}

// baselineRatchet is the exemption list, pinned here so that growing it
// is an edit to a test literal rather than a line in a data file.
//
// The store's baseline must stay a SUBSET of this. Deleting is the only
// legal edit: an entry comes off the list when the variant it names gets
// measured. Adding one excuses a model from the check, and that belongs
// in a diff a reviewer reads as what it is.
var baselineRatchet = []string{
	"gpt-oss-20b/mxfp4-gguf",
	"qwen3.5-27b/q4-gguf",
	"qwen3.5-2b/q4-gguf",
	"qwen3.5-35b-a3b/q4-gguf",
	"qwen3.5-4b/q4-gguf",
	"qwen3.5-9b/q4-gguf",
	"qwen3.6-27b/mtp-q4-gguf",
	"qwen3.6-27b/q4-gguf",
	"qwen3.6-35b-a3b/mtp-q4-gguf",
	"qwen3.6-35b-a3b/q4-gguf",
	"qwen3.8-27b/mtp-q4-gguf",
}

func TestBaselineOnlyShrinks(t *testing.T) {
	set, err := RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	for key := range set.Baseline {
		if !slices.Contains(baselineRatchet, key) {
			t.Errorf("baseline entry %q is not in baselineRatchet: excusing a variant from the "+
				"shape check is a deliberate edit to that literal, not a data-file change", key)
		}
	}
	// ...and the other direction, so the literal cannot accumulate dead
	// exemptions. `shapes --import` DELETES a variant's baseline entry
	// when it is measured; without this, the ratchet would keep listing
	// it and the list would slowly stop describing anything. The
	// baseline is allowed to reach zero — that is the mechanism working.
	for _, key := range baselineRatchet {
		if _, ok := set.Baseline[key]; !ok {
			t.Errorf("baselineRatchet lists %q but the store does not excuse it any more — "+
				"the variant was measured, so drop the entry", key)
		}
	}
}

// countRecords counts variant records across the store.
func countRecords(set RequestShapeSet) int {
	n := 0
	for _, m := range set.Models {
		n += len(m.Variants)
	}
	return n
}

// TestBaselineEntriesPinTheVariantTheyExcuse: an exemption names a
// variant AND the bytes it was granted against. Move the source tag and
// the exemption stops applying, which is the point of keying it by SHA.
func TestBaselineEntriesPinTheVariantTheyExcuse(t *testing.T) {
	set, err := RequestShapes()
	if err != nil {
		t.Fatalf("RequestShapes: %v", err)
	}
	all, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	for key, sha := range set.Baseline {
		if sha == "" {
			t.Errorf("baseline %q has no variant_sha", key)
			continue
		}
		var found bool
		for _, m := range all {
			for _, v := range m.Variants {
				if BaselineKey(m.ModelID, v.VariantID) == key {
					found = true
					if VariantSHA(v) != sha {
						t.Errorf("baseline %q pins %s but the shipped variant is %s", key, sha, VariantSHA(v))
					}
				}
			}
		}
		if !found {
			t.Errorf("baseline %q names no shipped variant", key)
		}
	}
}

func variantExists(manifests []Manifest, modelID, variantID string) bool {
	for _, m := range manifests {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return true
			}
		}
	}
	return false
}

// --- gap logic, against synthetic manifests -------------------------

func shapeManifest(modelID string, variants ...Variant) Manifest {
	return Manifest{ModelID: modelID, Variants: variants}
}

func ollamaVariant(id, tag string) Variant {
	return Variant{
		VariantID:      id,
		Format:         FormatOllamaTag,
		RuntimeSupport: []string{RuntimeOllama},
		Source:         VariantSource{Type: "ollama", Tag: tag},
	}
}

func wantShapes() []ShapeRef {
	return []ShapeRef{{Name: "leading-system", Digest: "aaa"}, {Name: "trailing-system", Digest: "bbb"}}
}

func fullRecord(sha string) VariantRequestShapes {
	return VariantRequestShapes{
		VariantSHA:    sha,
		Engine:        "ollama",
		EngineVersion: "0.32.15",
		AgentRevision: "abc123abc123",
		Retrieved:     "2026-08-28",
		Shapes: map[string]ShapeOutcome{
			"leading-system":  {Digest: "aaa", Outcome: ShapeAccepted, Status: 200, EngineSawRoles: []string{"system", "user"}},
			"trailing-system": {Digest: "bbb", Outcome: ShapeAccepted, Status: 200, EngineSawRoles: []string{"user", "system"}},
		},
	}
}

func TestRequestShapeGaps(t *testing.T) {
	v := ollamaVariant("q4-gguf", "m:q4")
	sha := VariantSHA(v)
	manifests := []Manifest{shapeManifest("m", v)}

	t.Run("no record is a gap", func(t *testing.T) {
		var set RequestShapeSet
		gaps := set.RequestShapeGaps(manifests, nil, wantShapes())
		if len(gaps) != 1 || gaps[0].Reason != "no shape record" {
			t.Fatalf("gaps = %+v", gaps)
		}
	})

	t.Run("a complete record is not a gap", func(t *testing.T) {
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": fullRecord(sha)}},
		}}
		if gaps := set.RequestShapeGaps(manifests, nil, wantShapes()); len(gaps) != 0 {
			t.Fatalf("gaps = %+v", gaps)
		}
	})

	t.Run("a missing row is a gap naming that row", func(t *testing.T) {
		rec := fullRecord(sha)
		delete(rec.Shapes, "trailing-system")
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": rec}},
		}}
		gaps := set.RequestShapeGaps(manifests, nil, wantShapes())
		if len(gaps) != 1 {
			t.Fatalf("gaps = %+v", gaps)
		}
		if want := `shape "trailing-system" was never measured`; gaps[0].Reason != want {
			t.Errorf("reason = %q, want %q", gaps[0].Reason, want)
		}
	})

	t.Run("a row measured at another digest is a gap for that row only", func(t *testing.T) {
		rec := fullRecord(sha)
		rec.Shapes["trailing-system"] = ShapeOutcome{Digest: "old", Outcome: ShapeAccepted, Status: 200, EngineSawRoles: []string{"user", "system"}}
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": rec}},
		}}
		gaps := set.RequestShapeGaps(manifests, nil, wantShapes())
		if len(gaps) != 1 {
			t.Fatalf("gaps = %+v", gaps)
		}
		if !regexp.MustCompile(`trailing-system.*digest old`).MatchString(gaps[0].Reason) {
			t.Errorf("reason should name the row and its digest: %q", gaps[0].Reason)
		}
	})

	t.Run("a record against another variant is stale", func(t *testing.T) {
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": fullRecord("some-other-sha")}},
		}}
		gaps := set.RequestShapeGaps(manifests, nil, wantShapes())
		if len(gaps) != 1 {
			t.Fatalf("gaps = %+v", gaps)
		}
	})

	t.Run("an unmeasurable model is skipped", func(t *testing.T) {
		var set RequestShapeSet
		gaps := set.RequestShapeGaps(manifests, map[string]string{"m": "no runner can host it"}, wantShapes())
		if len(gaps) != 0 {
			t.Fatalf("gaps = %+v", gaps)
		}
	})

	t.Run("a baseline entry at the same sha is skipped", func(t *testing.T) {
		set := RequestShapeSet{Baseline: map[string]string{"m/q4-gguf": sha}}
		if gaps := set.RequestShapeGaps(manifests, nil, wantShapes()); len(gaps) != 0 {
			t.Fatalf("gaps = %+v", gaps)
		}
	})

	t.Run("a baseline entry at another sha is a gap", func(t *testing.T) {
		set := RequestShapeSet{Baseline: map[string]string{"m/q4-gguf": "stale-sha"}}
		gaps := set.RequestShapeGaps(manifests, nil, wantShapes())
		if len(gaps) != 1 {
			t.Fatalf("gaps = %+v", gaps)
		}
		if !regexp.MustCompile(`different variant`).MatchString(gaps[0].Reason) {
			t.Errorf("reason = %q", gaps[0].Reason)
		}
	})

	t.Run("a model with no ollama variant is one model-level gap", func(t *testing.T) {
		vllmOnly := []Manifest{shapeManifest("v", Variant{
			VariantID:      "fp8",
			RuntimeSupport: []string{RuntimeVLLM},
			Source:         VariantSource{Type: "hf", RepoID: "org/model"},
		})}
		var set RequestShapeSet
		gaps := set.RequestShapeGaps(vllmOnly, nil, wantShapes())
		if len(gaps) != 1 || gaps[0].VariantID != "" {
			t.Fatalf("gaps = %+v", gaps)
		}
	})
}

func TestStaleEngineVersionsIsReportedButNotAGap(t *testing.T) {
	v := ollamaVariant("q4-gguf", "m:q4")
	sha := VariantSHA(v)
	manifests := []Manifest{shapeManifest("m", v)}
	rec := fullRecord(sha) // recorded on 0.32.15
	set := RequestShapeSet{Models: map[string]ModelRequestShapes{
		"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": rec}},
	}}

	if gaps := set.RequestShapeGaps(manifests, nil, wantShapes()); len(gaps) != 0 {
		t.Fatalf("a pin bump must not open a gap: %+v", gaps)
	}
	drift := set.StaleEngineVersions("0.32.16")
	if len(drift) != 1 {
		t.Fatalf("drift = %+v", drift)
	}
	if none := set.StaleEngineVersions("0.32.15"); len(none) != 0 {
		t.Fatalf("drift at the recorded pin = %+v", none)
	}
}

// TestRejectedShapes is the check that separates "there is evidence"
// from "the model works".
//
// RequestShapeGaps never reads an outcome, so before RejectedShapes
// existed a variant could be measured REFUSING the shape Claude Code
// sends and still report no gap. qwen3.8-27b is offered today and is the
// model the whole table exists for; its ollama 0.32.13 matrix refuses
// three rows. A presence-only gate would have filed that and shipped it.
func TestRejectedShapes(t *testing.T) {
	v := ollamaVariant("q4-gguf", "m:q4")
	sha := VariantSHA(v)
	manifests := []Manifest{shapeManifest("m", v)}

	refusing := func() VariantRequestShapes {
		rec := fullRecord(sha)
		rec.Shapes["trailing-system"] = ShapeOutcome{
			Digest: "bbb", Outcome: ShapeRejected, Status: 500,
			Marker: "shape_rejected", EngineSawRoles: []string{"user", "system"},
		}
		return rec
	}

	t.Run("a refused shape is reported", func(t *testing.T) {
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": refusing()}},
		}}
		got := set.RejectedShapes(manifests, nil)
		if len(got) != 1 {
			t.Fatalf("want 1 rejection, got %d: %+v", len(got), got)
		}
		if got[0].Shape != "trailing-system" || got[0].Status != 500 {
			t.Errorf("wrong rejection reported: %+v", got[0])
		}
		if got[0].EngineVersion != "0.32.15" {
			t.Errorf("a rejection has to name the engine it was measured on, got %q", got[0].EngineVersion)
		}
		// The same record must NOT also read as missing: one defect,
		// one finding.
		if gaps := set.RequestShapeGaps(manifests, nil, wantShapes()); len(gaps) != 0 {
			t.Errorf("a refused shape is not a coverage gap, got %+v", gaps)
		}
	})

	t.Run("an all-accepted record reports nothing", func(t *testing.T) {
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": fullRecord(sha)}},
		}}
		if got := set.RejectedShapes(manifests, nil); len(got) != 0 {
			t.Errorf("want no rejections, got %+v", got)
		}
	})

	t.Run("a missing record is the gap check's business, not this one's", func(t *testing.T) {
		empty := RequestShapeSet{}
		if got := empty.RejectedShapes(manifests, nil); len(got) != 0 {
			t.Errorf("absence must not be reported as a refusal, got %+v", got)
		}
	})

	t.Run("unmeasurable silences it, as it does the gap check", func(t *testing.T) {
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"q4-gguf": refusing()}},
		}}
		got := set.RejectedShapes(manifests, map[string]string{"m": "no runner can host it"})
		if len(got) != 0 {
			t.Errorf("want no rejections for an unmeasurable model, got %+v", got)
		}
	})

	t.Run("a vLLM-only variant is not probed here", func(t *testing.T) {
		vllm := Variant{VariantID: "fp8", Format: FormatSafetensors,
			RuntimeSupport: []string{RuntimeVLLM},
			Source:         VariantSource{Type: SourceHuggingFace, RepoID: "org/m"}}
		set := RequestShapeSet{Models: map[string]ModelRequestShapes{
			"m": {Variants: map[string]VariantRequestShapes{"fp8": refusing()}},
		}}
		if got := set.RejectedShapes([]Manifest{shapeManifest("m", vllm)}, nil); len(got) != 0 {
			t.Errorf("the matrix is measured on ollama; a vLLM variant has no row here, got %+v", got)
		}
	})
}
