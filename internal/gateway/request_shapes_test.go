package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// Table-driven cover of the request shapes real clients send
// (requestshapes.go).
//
// PRODUCT CONTRACT (waired-agent#1035, and #1055 for the native
// surface): whatever shape a client sends, the body this gateway hands
// an engine carries at most one instruction turn, and if it carries one
// it is first.
//
// "At most", not "exactly": when every instruction turn folds to empty
// text the fold leaves no system message behind, on the same reasoning
// both folds state — a contentless system message is worse than none
// (convert.go normalizeInstructionTurns, openai_instruction_turns.go).
//
// The limit of the native fold — a body whose instruction turn carries
// content it cannot merge is forwarded byte-identical — is pinned by
// TestNormalizeOpenAIBodyInstructionTurns_LeavesTheBodyAlone in
// openai_instruction_turns_test.go and is not restated here.

// instructionTurnPositions returns the indexes of the instruction turns
// in an engine-bound message list.
func instructionTurnPositions(t *testing.T, captured string) []int {
	t.Helper()
	var sent struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(captured), &sent); err != nil {
		t.Fatalf("decode engine-bound body: %v\nbody: %s", err, captured)
	}
	if len(sent.Messages) == 0 {
		t.Fatalf("engine-bound body carried no messages: %s", captured)
	}
	var at []int
	for i, m := range sent.Messages {
		if isInstructionRole(m.Role) {
			at = append(at, i)
		}
	}
	return at
}

func assertAtMostOneLeadingInstructionTurn(t *testing.T, shape, captured string) {
	t.Helper()
	at := instructionTurnPositions(t, captured)
	if len(at) > 1 {
		t.Errorf("%s: engine saw %d instruction turns at %v, want at most 1\nbody: %s",
			shape, len(at), at, captured)
		return
	}
	if len(at) == 1 && at[0] != 0 {
		t.Errorf("%s: engine saw an instruction turn at index %d, want index 0\nbody: %s",
			shape, at[0], captured)
	}
}

func TestClientShapes_EngineSeesAtMostOneLeadingInstructionTurn(t *testing.T) {
	shapes := ClientShapes()
	if len(shapes) < 4 {
		t.Fatalf("ClientShapes() returned %d rows; this test would be checking nothing", len(shapes))
	}
	for _, s := range shapes {
		t.Run(s.Name, func(t *testing.T) {
			body, err := s.AnthropicBody("waired/default")
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			var captured string
			upstream := fakeOllamaForAnthropic(t, &captured)
			defer upstream.Close()

			w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), string(body))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			assertAtMostOneLeadingInstructionTurn(t, s.Name, captured)
		})
	}
}

func TestEngineShapes_NativeSurfaceSeesAtMostOneLeadingInstructionTurn(t *testing.T) {
	shapes := EngineShapes()
	if len(shapes) < 6 {
		t.Fatalf("EngineShapes() returned %d rows; this test would be checking nothing", len(shapes))
	}
	for _, s := range shapes {
		t.Run(s.Name, func(t *testing.T) {
			body, err := s.OpenAIBody("waired/default")
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			var captured string
			upstream := fakeOllama(t, &captured)
			defer upstream.Close()

			w := postOpenAIBody(t, openAIGatewayFor(t, upstream.URL), string(body))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			assertAtMostOneLeadingInstructionTurn(t, s.Name, captured)
		})
	}
}

// TestRequestShapeTablesCarryTheShapeThatBrokeUs keeps the tables from
// being emptied of the one row they exist for. #1035 was a non-leading
// instruction turn; a table with no such row would pass every test
// above while asserting nothing.
func TestRequestShapeTablesCarryTheShapeThatBrokeUs(t *testing.T) {
	engineHas := false
	for _, s := range EngineShapes() {
		for i, role := range s.Roles {
			if i > 0 && isInstructionRole(role) {
				engineHas = true
			}
		}
	}
	if !engineHas {
		t.Error("no EngineShapes row carries a non-leading instruction turn (#1035)")
	}

	clientHas := false
	for _, s := range ClientShapes() {
		for i, role := range s.MessageRoles {
			if i > 0 && isInstructionRole(role) {
				clientHas = true
			}
		}
	}
	if !clientHas {
		t.Error("no ClientShapes row carries a non-leading instruction turn (#1035)")
	}
}

func TestRequestShapeDigestsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, s := range EngineShapes() {
		d := s.Digest()
		if prev, dup := seen[d]; dup {
			t.Errorf("engine shape %q and %q share digest %s", s.Name, prev, d)
		}
		seen[d] = s.Name
	}
	for _, s := range ClientShapes() {
		d := s.Digest()
		if prev, dup := seen[d]; dup {
			t.Errorf("client shape %q and %q share digest %s", s.Name, prev, d)
		}
		seen[d] = s.Name
	}
}

// TestRequestShapeDigestTracksTheRow is the canary for the digest
// itself: a mislabelled chunk, or a field left out of the hash, makes
// the digest constant, and a constant digest silently accepts a stored
// measurement of a shape nobody is sending any more.
func TestRequestShapeDigestTracksTheRow(t *testing.T) {
	base := EngineShapes()[0]

	renamed := base
	renamed.Name = base.Name + "-renamed"
	if renamed.Digest() == base.Digest() {
		t.Error("engine shape digest ignored Name")
	}

	reroled := base
	reroled.Roles = append(slices.Clone(base.Roles), RoleSystem)
	if reroled.Digest() == base.Digest() {
		t.Error("engine shape digest ignored Roles")
	}

	// Why is prose about the row, not the row. Editing it must not
	// retire a measurement.
	rewritten := base
	rewritten.Why = base.Why + " (reworded)"
	if rewritten.Digest() != base.Digest() {
		t.Error("engine shape digest included Why; editing prose would invalidate stored evidence")
	}

	cbase := ClientShapes()[0]
	cblocks := cbase
	cblocks.TopLevelSystemBlocks = cbase.TopLevelSystemBlocks + 1
	if cblocks.Digest() == cbase.Digest() {
		t.Error("client shape digest ignored TopLevelSystemBlocks")
	}
	cform := cbase
	cform.TrailingSystemAsBlockArray = !cbase.TrailingSystemAsBlockArray
	if cform.Digest() == cbase.Digest() {
		t.Error("client shape digest ignored TrailingSystemAsBlockArray")
	}
	cbeta := cbase
	cbeta.BetaHeader = cbase.BetaHeader + "-x"
	if cbeta.Digest() == cbase.Digest() {
		t.Error("client shape digest ignored BetaHeader")
	}
}

// TestEngineShapeBodiesAreWellFormed keeps a rendering bug from being
// recorded as a model's answer: a body an engine rejects for its tool
// definition, or for a missing field, would file the wrong finding
// against the model.
func TestEngineShapeBodiesAreWellFormed(t *testing.T) {
	for _, s := range EngineShapes() {
		t.Run(s.Name, func(t *testing.T) {
			raw, err := s.OpenAIBody("m")
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var req OpenAIRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.MaxTokens != 1 {
				t.Errorf("max_tokens = %d, want 1 (the question is whether the template renders, not what the model says)", req.MaxTokens)
			}
			if len(req.Messages) != len(s.Roles) {
				t.Fatalf("rendered %d messages, want %d", len(req.Messages), len(s.Roles))
			}
			wantRoles := s.EngineRoles()
			for i, m := range req.Messages {
				if m.Role != wantRoles[i] {
					t.Errorf("messages[%d].role = %q, want %q", i, m.Role, wantRoles[i])
				}
			}
			// Every tool call must be answered, and every tool result
			// must answer a call: an engine that validates the pairing
			// would otherwise reject the row for the pairing.
			var callIDs, resultIDs []string
			for _, m := range req.Messages {
				for _, tc := range m.ToolCalls {
					callIDs = append(callIDs, tc.ID)
				}
				if m.Role == RoleTool {
					resultIDs = append(resultIDs, m.ToolCallID)
				}
			}
			if !slices.Equal(callIDs, resultIDs) {
				t.Errorf("tool call ids %v do not match tool result ids %v", callIDs, resultIDs)
			}
			if len(callIDs) > 0 && len(req.Tools) == 0 {
				t.Error("the row calls a tool but offers none")
			}
		})
	}
}

func TestClientShapeBodiesAreWellFormed(t *testing.T) {
	for _, s := range ClientShapes() {
		t.Run(s.Name, func(t *testing.T) {
			raw, err := s.AnthropicBody("m")
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var req AnthropicRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if req.MaxTokens != 1 {
				t.Errorf("max_tokens = %d, want 1", req.MaxTokens)
			}
			if len(req.Messages) != len(s.MessageRoles) {
				t.Fatalf("rendered %d messages, want %d", len(req.Messages), len(s.MessageRoles))
			}
			if s.TopLevelSystemBlocks == 0 {
				if len(req.System) != 0 {
					t.Errorf("row declares no top-level system blocks but rendered %s", req.System)
				}
				return
			}
			var blocks []map[string]any
			if err := json.Unmarshal(req.System, &blocks); err != nil {
				t.Fatalf("top-level system is not a block array: %v", err)
			}
			if len(blocks) != s.TopLevelSystemBlocks {
				t.Errorf("top-level system has %d blocks, want %d", len(blocks), s.TopLevelSystemBlocks)
			}
		})
	}
}
