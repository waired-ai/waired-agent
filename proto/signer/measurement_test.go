package signer

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestInferenceState_ModelMeasurements_CanonicalJSON is the byte-identity
// pin required of every additive proto change
// (docs/decisions/20260719/0000-concurrent-proto-development.md §3), for
// the waired-agent#970 additions.
//
// It carries the weight the earlier pins do: InferenceState rides the
// signed NetworkMap, so a device that reports neither must encode
// byte-for-byte as it does today or every existing signature stops
// verifying on a rolling upgrade. That is the whole fleet until the
// agent-side producer ships, and every agent older than it afterwards.
func TestInferenceState_ModelMeasurements_CanonicalJSON(t *testing.T) {
	silent := InferenceState{
		Reachable: true,
		Type:      InferenceTypeOllama,
		Endpoint:  "http://127.0.0.1:11434",
		LastCheck: "2026-08-21T00:00:00Z",
	}
	const wantSilent = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-21T00:00:00Z"}`
	data, err := json.Marshal(&silent)
	if err != nil {
		t.Fatalf("marshal without measurements: %v", err)
	}
	if got := string(data); got != wantSilent {
		t.Errorf("a device reporting no measurements changed the encoding:\n got %s\nwant %s",
			got, wantSilent)
	}

	// An empty SLICE must encode as absent too, not as `[]`. A producer
	// that builds the slice and finds nothing to put in it is the same
	// claim as a producer that never ran, and `"model_measurements":[]`
	// would be a different byte string on the signed map.
	empty := silent
	empty.ModelMeasurements = []ModelMeasurement{}
	data, err = json.Marshal(&empty)
	if err != nil {
		t.Fatalf("marshal with an empty slice: %v", err)
	}
	if got := string(data); got != wantSilent {
		t.Errorf("an empty measurement slice changed the encoding:\n got %s\nwant %s",
			got, wantSilent)
	}

	// Reporting: the two keys sit at the end, in struct-declaration
	// order, after the residency pair that precedes them.
	reporting := silent
	reporting.ServingEngineVersion = "0.32.13"
	reporting.ModelMeasurements = []ModelMeasurement{{
		ModelID:       "qwen3.5-9b",
		VariantID:     "q4-gguf",
		DecodeTokps:   11,
		Method:        BenchmarkMethodOllamaEval,
		EngineKind:    "ollama",
		EngineVersion: "0.32.13",
		MeasuredAt:    "2026-08-21T00:00:00Z",
	}}
	const wantReporting = `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-21T00:00:00Z",` +
		`"model_measurements":[{"model_id":"qwen3.5-9b","variant_id":"q4-gguf",` +
		`"decode_tokps":11,"method":"ollama_eval","engine_kind":"ollama",` +
		`"engine_version":"0.32.13","measured_at":"2026-08-21T00:00:00Z"}],` +
		`"serving_engine_version":"0.32.13"}`
	data, err = json.Marshal(&reporting)
	if err != nil {
		t.Fatalf("marshal while reporting: %v", err)
	}
	if got := string(data); got != wantReporting {
		t.Errorf("reporting encoding drifted:\n got %s\nwant %s", got, wantReporting)
	}

	// Round-trips: a consumer that decodes and re-encodes must produce
	// the same bytes, which is what the signed map depends on.
	var back InferenceState
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back.ModelMeasurements, reporting.ModelMeasurements) {
		t.Errorf("measurements did not survive the round trip:\n got %+v\nwant %+v",
			back.ModelMeasurements, reporting.ModelMeasurements)
	}
	if back.ServingEngineVersion != "0.32.13" {
		t.Errorf("serving engine version did not survive: %q", back.ServingEngineVersion)
	}
	again, err := json.Marshal(&back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != wantReporting {
		t.Errorf("re-marshal is not byte-identical:\n got %s\nwant %s", again, wantReporting)
	}
}

// PRODUCT CONTRACT (waired-agent#970): an older agent that does not know
// these fields must not be able to change the map's bytes by decoding
// and re-encoding it. This is the failure the strip requirement exists
// for, expressed as a test rather than as a comment.
func TestInferenceState_UnknownFieldsAreDroppedOnReMarshal(t *testing.T) {
	// What an older agent's decoder does: it has no field for these two,
	// so json.Unmarshal discards them, and its re-marshal is missing
	// them. Modelled with a struct that predates them.
	type olderInferenceState struct {
		Reachable bool   `json:"reachable"`
		Type      string `json:"type"`
		Endpoint  string `json:"endpoint"`
		LastCheck string `json:"last_check"`
	}
	withFields := `{"reachable":true,"type":"ollama","endpoint":"http://127.0.0.1:11434",` +
		`"last_check":"2026-08-21T00:00:00Z",` +
		`"model_measurements":[{"model_id":"qwen3.5-9b","decode_tokps":11}],` +
		`"serving_engine_version":"0.32.13"}`

	var older olderInferenceState
	if err := json.Unmarshal([]byte(withFields), &older); err != nil {
		t.Fatalf("older decode: %v", err)
	}
	again, err := json.Marshal(&older)
	if err != nil {
		t.Fatalf("older re-marshal: %v", err)
	}
	if string(again) == withFields {
		t.Fatal("the fixture does not model the hazard: an older agent re-marshalled them unchanged")
	}
	// This is why they MUST be stripped before the map is served and
	// signed: an agent that predates them re-marshals to different bytes
	// and the whole map's signature stops verifying — not just this
	// entry's.
}
