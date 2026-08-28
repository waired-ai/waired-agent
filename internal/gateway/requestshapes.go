package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strings"
)

// Request shapes: the message-role sequences a coding agent actually
// sends, and the sequences an engine's chat template has to accept.
//
// Why this table exists (waired-agent#1035): qwen3.8-27b shipped with a
// passing agent-harness verdict and then failed every real Claude Code
// turn on a 24 GB host. The probe could not have caught it — its
// fixture builds exactly [user, assistant, user] (agentgrade
// fixture.go) and its digest does not hash the role sequence at all
// (agentgrade revision.go), so the one dimension that broke was the one
// dimension nothing asserted.
//
// Two tables, because they answer two different questions:
//
//   - EngineShapes  — what an engine's chat template accepts. Posted
//     straight at /v1/chat/completions, this is a fact about
//     (weights, template, engine version) and stays true no matter what
//     this gateway does to a request later.
//   - ClientShapes  — what a coding agent puts on the wire. Posted at
//     the Anthropic surface, it says whether the whole path holds for a
//     real client.
//
// Both are written here rather than captured. A real capture carries the
// client's own identifiers (Claude Code sends metadata.user_id with a
// device id and a session id) and a third-party vendor's system prompt,
// and this repository is public — the same reasons agentgrade's fixture
// gives for authoring its request instead of recording one. What is
// taken from the measurement is the shape: the role sequence, the block
// counts, and the beta header. Filler text below is ours.

// Engine-facing message roles. These are the strings an OpenAI-shaped
// chat template dispatches on, so they are spelled exactly.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleDeveloper = "developer"

	// RoleAssistantToolCalls is an assistant turn carrying tool_calls,
	// and RoleTool is the tool result that answers it. They are one
	// token here because neither is well formed without the other.
	RoleAssistantToolCalls = "assistant+tool_calls"
	RoleTool               = "tool"
)

// EngineShape is one message-role sequence as an engine's chat template
// sees it.
type EngineShape struct {
	// Name identifies the row in a stored record and in a failure
	// message. It is part of the digest, so renaming a row retires the
	// old row's evidence rather than silently re-labelling it.
	Name string

	// Roles is the engine-facing message sequence, in order.
	Roles []string

	// Why records what this row is for, and is deliberately NOT part of
	// the digest: editing the prose must not invalidate a measurement.
	Why string
}

// EngineShapes returns the shapes an engine's chat template must
// accept, measured on a live engine rather than derived from a spec.
//
// The six rows and their measured outcomes are recorded verbatim in
// docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md §1
// (sv-mag, ollama 0.32.13, qwen3.8:27b-mtp-q4_K_M, 2026-08-27):
// rows 3, 4 and 5 answered 500 "system message must be at the
// beginning" on that model, and qwen3.6-35b-a3b and qwen3.5-9b answered
// 200 to all six on the same engine on the same day. That per-model
// difference is the whole reason this is measured per model.
func EngineShapes() []EngineShape {
	return []EngineShape{
		{
			Name:  "leading-system",
			Roles: []string{RoleSystem, RoleUser},
			Why:   "the ordinary shape; a template that rejects this rejects everything",
		},
		{
			Name:  "no-system",
			Roles: []string{RoleUser},
			Why:   "a bare turn, as a warm-up probe sends it",
		},
		{
			Name:  "trailing-system",
			Roles: []string{RoleUser, RoleSystem},
			Why:   "what Claude Code sends under mid-conversation-system-2026-04-07 (#1035)",
		},
		{
			Name:  "double-system",
			Roles: []string{RoleSystem, RoleSystem, RoleUser},
			Why:   "a second instruction turn adjacent to the first",
		},
		{
			Name:  "system-after-tool-roundtrip",
			Roles: []string{RoleSystem, RoleUser, RoleAssistantToolCalls, RoleTool, RoleSystem, RoleUser},
			Why:   "the #1035 shape once the conversation has run a tool",
		},
		{
			Name:  "developer-turn",
			Roles: []string{RoleSystem, RoleUser, RoleDeveloper, RoleUser},
			Why:   "the other instruction role; folded by the same rule",
		},
	}
}

// Digest is a stable identifier for one row's content.
//
// Per row, not one digest over the whole table, and this is load
// bearing: a single table-wide revision would make adding a row
// invalidate every stored measurement and force a GPU re-run of the
// whole catalog — the mass invalidation FixtureRevision causes in
// agentgrade, and the thing this mechanism is required not to do. With
// a per-row digest, adding a row leaves every other row's evidence
// standing and asks only for the new row.
//
// Why is excluded on purpose (see the field comment).
//
// The RENDERED body is hashed, not just the name and roles. What an
// engine accepts or rejects is the whole request, and OpenAIBody decides
// more than the role sequence: max_tokens, the filler text, and — load
// bearing — whether a tools array rides along, which is the difference
// between a 400 about the role order and a 400 about a tool call naming
// a tool that was never offered. Hashing only the roles would let the
// tool schema change while every stored "accepted" kept claiming to
// answer the question now being asked. agentgrade.FixtureRevision hashes
// its rendered tool set for exactly this reason.
//
// The body is rendered against a fixed placeholder model so the digest
// describes the shape rather than the subject.
func (s EngineShape) Digest() string {
	h := sha256.New()
	shapeChunk(h, "name", []byte(s.Name))
	shapeChunk(h, "roles", []byte(strings.Join(s.Roles, ",")))
	body, err := s.OpenAIBody(shapeDigestModel)
	if err != nil {
		// A row that cannot render is a broken table, caught by
		// TestEngineShapeBodiesAreWellFormed. Fold the error in so the
		// digest stays deterministic rather than panicking here.
		shapeChunk(h, "render-error", []byte(err.Error()))
	} else {
		shapeChunk(h, "body", body)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// shapeDigestModel stands in for the model under test when a row is
// hashed. Any fixed string works; it must never be a real catalog tag,
// or the digest would change with the subject and every record would go
// stale on the next model.
const shapeDigestModel = "waired-shape-digest"

// OpenAIBody renders the shape as a request body for an OpenAI-shaped
// chat endpoint.
//
// max_tokens is 1: the question is whether the template renders the
// conversation at all, not what the model says. A row that gets a 200
// has answered it.
func (s EngineShape) OpenAIBody(model string) ([]byte, error) {
	req := OpenAIRequest{
		Model:     model,
		MaxTokens: 1,
	}
	toolCallID := "call_shape_probe"
	for i, role := range s.Roles {
		switch role {
		case RoleSystem, RoleDeveloper:
			req.Messages = append(req.Messages, OpenAIMessage{
				Role:    role,
				Content: shapeFiller(role, i),
			})
		case RoleUser, RoleAssistant:
			req.Messages = append(req.Messages, OpenAIMessage{
				Role:    role,
				Content: shapeFiller(role, i),
			})
		case RoleAssistantToolCalls:
			req.Messages = append(req.Messages, OpenAIMessage{
				Role: RoleAssistant,
				ToolCalls: []OpenAIToolCall{{
					ID:   toolCallID,
					Type: "function",
					Function: OpenAIToolCallFunction{
						Name:      shapeToolName,
						Arguments: `{"path":"/etc/hostname"}`,
					},
				}},
			})
		case RoleTool:
			req.Messages = append(req.Messages, OpenAIMessage{
				Role:       RoleTool,
				ToolCallID: toolCallID,
				Content:    "shape-probe tool result",
			})
		default:
			return nil, fmt.Errorf("request shape %q: unknown role %q", s.Name, role)
		}
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("request shape %q: no messages", s.Name)
	}
	// A shape that names a tool call has to offer the tool, or a strict
	// engine rejects the request for the tool definition rather than for
	// the role sequence — and the row would record the wrong finding.
	if s.usesTools() {
		req.Tools = []OpenAITool{{
			Type: "function",
			Function: OpenAIToolFunction{
				Name:        shapeToolName,
				Description: "Read a file. Offered so a tool_calls turn is well formed.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		}}
	}
	return json.Marshal(req)
}

// EngineRoles returns the roles as they appear on the engine-bound
// message list, with the tool-call pair spelled the way the wire spells
// it. A recorded measurement compares this against what the engine
// actually received.
func (s EngineShape) EngineRoles() []string {
	out := make([]string, 0, len(s.Roles))
	for _, role := range s.Roles {
		if role == RoleAssistantToolCalls {
			out = append(out, RoleAssistant)
			continue
		}
		out = append(out, role)
	}
	return out
}

func (s EngineShape) usesTools() bool {
	for _, role := range s.Roles {
		if role == RoleAssistantToolCalls || role == RoleTool {
			return true
		}
	}
	return false
}

// ClientShape is one request as a coding agent puts it on the Anthropic
// surface.
type ClientShape struct {
	Name string

	// TopLevelSystemBlocks is the length of the top-level `system` block
	// array. Claude Code always sends the array form, never a string.
	TopLevelSystemBlocks int

	// MessageRoles is the `messages[]` role sequence.
	MessageRoles []string

	// TrailingSystemAsBlockArray picks between the two observed content
	// forms of a mid-conversation system turn: a one-element array of a
	// text block, or a bare string. Both are on the wire from the same
	// client build to different hosts, so both are shapes we accept.
	TrailingSystemAsBlockArray bool

	// BetaHeader is the anthropic-beta value that arms the shape, or ""
	// when the shape needs none.
	BetaHeader string

	Why string
}

// MidConversationSystemBeta is the beta that puts a second instruction
// turn inside messages[]. Present on every captured Claude Code request
// measured on 2026-08-25 (2.1.229 and 2.1.241).
const MidConversationSystemBeta = "mid-conversation-system-2026-04-07"

// ClientShapes returns the Anthropic-surface request shapes coding
// agents send.
//
// Derived from the shape of nine Claude Code requests captured on
// 2026-08-25 (client builds 2.1.229 and 2.1.241): every one carried a
// three-block top-level `system` with cache_control on blocks 2 and 3,
// a `messages` sequence of [user, system], and
// mid-conversation-system-2026-04-07 in anthropic-beta. Only the shape
// is reproduced here; see the file header.
func ClientShapes() []ClientShape {
	return []ClientShape{
		{
			Name:                 "claude-code-mid-conversation-system-blocks",
			TopLevelSystemBlocks: 3,
			MessageRoles:         []string{RoleUser, RoleSystem},

			TrailingSystemAsBlockArray: true,
			BetaHeader:                 MidConversationSystemBeta,
			Why:                        "measured 2026-08-25; the shape that broke every turn in #1035",
		},
		{
			Name:                       "claude-code-mid-conversation-system-string",
			TopLevelSystemBlocks:       3,
			MessageRoles:               []string{RoleUser, RoleSystem},
			TrailingSystemAsBlockArray: false,
			BetaHeader:                 MidConversationSystemBeta,
			Why:                        "same client build, other observed content form of the same turn",
		},
		{
			Name:                 "claude-code-warmup",
			TopLevelSystemBlocks: 0,
			MessageRoles:         []string{RoleUser},
			Why:                  "the 316-byte probe a client sends before its first real turn",
		},
		{
			Name:                       "instruction-turn-after-tool-roundtrip",
			TopLevelSystemBlocks:       3,
			MessageRoles:               []string{RoleUser, RoleAssistantToolCalls, RoleTool, RoleSystem, RoleUser},
			TrailingSystemAsBlockArray: true,
			BetaHeader:                 MidConversationSystemBeta,
			Why:                        "the same instruction turn once the conversation has run a tool",
		},
	}
}

// Digest is a stable identifier for one row's content. Why is excluded;
// see EngineShape.Digest.
func (s ClientShape) Digest() string {
	h := sha256.New()
	shapeChunk(h, "name", []byte(s.Name))
	shapeChunk(h, "roles", []byte(strings.Join(s.MessageRoles, ",")))
	shapeChunk(h, "sysblocks", fmt.Appendf(nil, "%d", s.TopLevelSystemBlocks))
	shapeChunk(h, "trailing", fmt.Appendf(nil, "%t", s.TrailingSystemAsBlockArray))
	shapeChunk(h, "beta", []byte(s.BetaHeader))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// AnthropicBody renders the shape as a POST /v1/messages body.
func (s ClientShape) AnthropicBody(model string) ([]byte, error) {
	req := AnthropicRequest{
		Model:     model,
		MaxTokens: 1,
	}
	if s.TopLevelSystemBlocks > 0 {
		blocks := make([]map[string]any, 0, s.TopLevelSystemBlocks)
		for i := range s.TopLevelSystemBlocks {
			b := map[string]any{"type": "text", "text": shapeFiller("system", i)}
			// cache_control rides every block but the first, which is
			// how the captured requests carry it.
			if i > 0 {
				b["cache_control"] = map[string]any{"type": "ephemeral"}
			}
			blocks = append(blocks, b)
		}
		raw, err := json.Marshal(blocks)
		if err != nil {
			return nil, err
		}
		req.System = raw
	}

	toolUseID := "toolu_shape_probe"
	for i, role := range s.MessageRoles {
		switch role {
		case RoleUser, RoleAssistant:
			content, err := json.Marshal(shapeFiller(role, i))
			if err != nil {
				return nil, err
			}
			req.Messages = append(req.Messages, AnthropicMessage{Role: role, Content: content})
		case RoleSystem, RoleDeveloper:
			content, err := s.instructionTurnContent(role, i)
			if err != nil {
				return nil, err
			}
			req.Messages = append(req.Messages, AnthropicMessage{Role: role, Content: content})
		case RoleAssistantToolCalls:
			content, err := json.Marshal([]map[string]any{{
				"type":  "tool_use",
				"id":    toolUseID,
				"name":  shapeToolName,
				"input": map[string]any{"path": "/etc/hostname"},
			}})
			if err != nil {
				return nil, err
			}
			req.Messages = append(req.Messages, AnthropicMessage{Role: RoleAssistant, Content: content})
		case RoleTool:
			// A tool result rides a user turn on the Anthropic wire.
			content, err := json.Marshal([]map[string]any{{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     "shape-probe tool result",
			}})
			if err != nil {
				return nil, err
			}
			req.Messages = append(req.Messages, AnthropicMessage{Role: RoleUser, Content: content})
		default:
			return nil, fmt.Errorf("client shape %q: unknown role %q", s.Name, role)
		}
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("client shape %q: no messages", s.Name)
	}
	if s.usesTools() {
		req.Tools = []AnthropicTool{{
			Name:        shapeToolName,
			Description: "Read a file. Offered so a tool_use turn is well formed.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}
	}
	return json.Marshal(req)
}

func (s ClientShape) instructionTurnContent(role string, i int) (json.RawMessage, error) {
	text := shapeFiller(role, i)
	if !s.TrailingSystemAsBlockArray {
		return json.Marshal(text)
	}
	return json.Marshal([]map[string]any{{
		"type":          "text",
		"text":          text,
		"cache_control": map[string]any{"type": "ephemeral"},
	}})
}

func (s ClientShape) usesTools() bool {
	for _, role := range s.MessageRoles {
		if role == RoleAssistantToolCalls || role == RoleTool {
			return true
		}
	}
	return false
}

// shapeToolName is the one tool a shape offers. Named for what it does
// rather than after any particular client's tool, so the table does not
// quietly become a copy of somebody else's tool set.
const shapeToolName = "read_file"

// shapeFiller is our own text, standing in for the client's. It names
// its own role and position so a body read in a log says which turn it
// came from.
func shapeFiller(role string, i int) string {
	return fmt.Sprintf("waired request-shape probe: %s turn %d", role, i)
}

// shapeChunk length-prefixes each digest input so two fields cannot
// alias into the same byte stream.
//
// hash.Hash.Write never returns an error, which is why the errors are
// discarded explicitly rather than left to drift.
func shapeChunk(h hash.Hash, label string, b []byte) {
	_, _ = fmt.Fprintf(h, "%s:%d:", label, len(b))
	_, _ = h.Write(b)
}
