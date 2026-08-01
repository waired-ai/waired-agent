package agentgrade

import (
	"encoding/json"
	"testing"
)

// schemaDepth is the maximum nesting depth of a decoded JSON value.
func schemaDepth(v any) int {
	switch t := v.(type) {
	case map[string]any:
		best := 0
		for _, child := range t {
			if d := schemaDepth(child); d > best {
				best = d
			}
		}
		return best + 1
	case []any:
		best := 0
		for _, child := range t {
			if d := schemaDepth(child); d > best {
				best = d
			}
		}
		return best + 1
	default:
		return 0
	}
}

// TestFixtureMatchesRealShape pins the fixture to the band measured off
// a real coding agent's request (see fixture.go for the reference
// numbers and how they were obtained).
//
// It is a floor check, not an equality check. The fixture does not have
// to be byte-identical to any client's request — it has to stay heavy
// enough that a model which passes here would not fall over on a real
// one. Trimming the fixture to make a model pass is the failure mode
// this guards.
func TestFixtureMatchesRealShape(t *testing.T) {
	tools, err := fixtureTools()
	if err != nil {
		t.Fatalf("fixtureTools: %v", err)
	}

	if len(tools) < fixtureMinTools {
		t.Errorf("tool count = %d, want >= %d", len(tools), fixtureMinTools)
	}

	totalBytes := 0
	maxDepth := 0
	for _, tool := range tools {
		enc, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal tool %s: %v", tool.Name, err)
		}
		totalBytes += len(enc)

		var schema any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %s: input_schema is not valid JSON: %v", tool.Name, err)
		}
		if d := schemaDepth(schema); d > maxDepth {
			maxDepth = d
		}
	}

	if totalBytes < fixtureMinToolBytes {
		t.Errorf("total tool schema bytes = %d, want >= %d", totalBytes, fixtureMinToolBytes)
	}
	if maxDepth < fixtureMinSchemaDepth {
		t.Errorf("max schema depth = %d, want >= %d", maxDepth, fixtureMinSchemaDepth)
	}
	if len(fixtureSystemPrompt) < fixtureMinSystemBytes {
		t.Errorf("system prompt = %d bytes, want >= %d", len(fixtureSystemPrompt), fixtureMinSystemBytes)
	}

	req, err := BuildRequest("test-model", Cases[0])
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	whole, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if len(whole) < fixtureMinRequestBytes {
		t.Errorf("whole request = %d bytes, want >= %d", len(whole), fixtureMinRequestBytes)
	}
	t.Logf("fixture shape: %d tools, %d B tool schemas, %d B system, depth %d, %d B whole request",
		len(tools), totalBytes, len(fixtureSystemPrompt), maxDepth, len(whole))
}

// Every tool name must be unique — a duplicate would make the
// hallucination check in Classify silently forgiving for that name, and
// engines reject duplicate function names outright.
func TestFixtureToolNamesUnique(t *testing.T) {
	names, err := ToolNames()
	if err != nil {
		t.Fatalf("ToolNames: %v", err)
	}
	tools, err := fixtureTools()
	if err != nil {
		t.Fatalf("fixtureTools: %v", err)
	}
	if len(names) != len(tools) {
		t.Errorf("%d unique names across %d tools — duplicate tool name", len(names), len(tools))
	}
	for _, tool := range tools {
		if tool.Name == "" {
			t.Error("a tool has an empty name")
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
}

// The fixture is a test input for third-party models and lives in a
// public repository. It must not carry client identifiers: the real
// capture that established the reference shape included a device id and
// a session id in metadata.user_id, and that is exactly what must never
// reach a checked-in fixture.
func TestFixtureCarriesNoClientIdentity(t *testing.T) {
	req, err := BuildRequest("test-model", Cases[0])
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(req.Metadata) != 0 {
		t.Errorf("request carries metadata %q; the fixture must send none", req.Metadata)
	}
}

// Every case must state what it probes. A case whose expectation nobody
// can explain is a case nobody can act on when it goes red.
func TestCasesAreDocumented(t *testing.T) {
	if len(Cases) == 0 {
		t.Fatal("no probe cases defined")
	}
	seen := map[string]bool{}
	wantsCall := 0
	for _, c := range Cases {
		if c.Name == "" || c.Prompt == "" || c.Why == "" {
			t.Errorf("case %q: name, prompt, and why are all required", c.Name)
		}
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		if c.WantToolCall {
			wantsCall++
		}
	}
	// Both directions must be probed: a model that never calls tools
	// passes an all-greeting suite, and the rc7 model passed the
	// tool-required direction while failing the greeting.
	if wantsCall == 0 || wantsCall == len(Cases) {
		t.Errorf("%d of %d cases require a tool call; the suite must probe both directions",
			wantsCall, len(Cases))
	}
}
