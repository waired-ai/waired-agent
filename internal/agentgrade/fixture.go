package agentgrade

import (
	"encoding/json"
	"fmt"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// The fixture is a coding-agent-shaped request: many complex tool
// schemas, a large system prompt, and a session's worth of accumulated
// context. That SHAPE is what breaks format compliance — #322's failure
// appeared under "~15 complex tool schemas + very large system prompt"
// and not on a bare two-tool request — so the probe has to reproduce the
// scale, not just the protocol.
//
// It is written here rather than captured from a coding agent's wire
// traffic, for two reasons that are not stylistic:
//
//   - a real capture carries the client's own identifiers (Claude Code
//     sends metadata.user_id with a device id and session id), and this
//     repository is public: "never commit real device identifiers,
//     including in test fixtures";
//   - the system prompt of a third-party agent is that vendor's text,
//     and vendoring ~9 KB of it verbatim into a public repo to use as a
//     test input is not ours to do.
//
// What IS captured is the shape: scripts/dev/measure-agent-request.py
// drives the real agent against a recording stub and reports the tool
// count, schema bytes, nesting depth, and system-prompt size — and
// nothing else, deliberately — and TestFixtureMatchesRealShape asserts
// this fixture stays in that band. So the numbers below are measured,
// and only the prose is ours.
//
// Measured reference (Claude Code 2.1.220, 2026-08-01, in an empty
// working directory):
//
//	tools                27
//	tool_schema_bytes    84,925
//	max_schema_depth     8
//	system_bytes         9,214
//	whole_request_bytes  101,801
//
// Re-measure with:
//
//	python3 scripts/dev/measure-agent-request.py -- claude -p hello
//
// The floors below sit at or just under what this fixture actually
// produces, which is ~68% of the reference total. They are NOT set to
// the reference itself, for a reason worth stating: the reference's
// tool-schema bulk is concentrated in three unusually large tools
// (21.8 KB, 9.3 KB, 7.7 KB — a third of the total between them) that
// belong to one installation's feature set rather than to coding agents
// generally. Padding this fixture with filler prose to match that number
// would buy a number, not pressure.
//
// What justifies the level is the reproduction check, not the ratio: the
// probe must classify a known-bad model as failing at this size (see the
// live-model tests). If a model that fails in real use passes here, the
// fixture is too small and these floors go up — that is the signal to
// act on, not the percentage.
const (
	// fixtureMinTools is the tool-count floor. #322 observed the failure
	// at ~15 schemas; the reference client sends 27. Below this the
	// request stops exercising tool-choice pressure at all.
	fixtureMinTools = 15

	// fixtureMinSystemBytes is the system-prompt floor, a little under
	// the measured 9,214 B so ordinary editing does not trip the guard.
	fixtureMinSystemBytes = 8000

	// fixtureMinToolBytes is the floor on total serialised tool schemas.
	fixtureMinToolBytes = 25000

	// fixtureMinSchemaDepth is the nesting floor. The reference client
	// reaches 8; a flat schema set would not exercise the template's
	// rendering of nested objects and arrays at all.
	fixtureMinSchemaDepth = 7

	// fixtureMinRequestBytes is the floor on the whole serialised
	// request. This is the headline number: total weight is what a model
	// has to hold while still emitting exact syntax, and it is the one
	// that most directly separates "reproduces the failure" from
	// "measures an easier problem than the user has".
	fixtureMinRequestBytes = 60000
)

// fixtureModel is the placeholder model id in the built request. The
// caller overwrites it with the model under test; the gateway resolves
// it through the ordinary catalog/alias path.
const fixtureModel = "waired/default"

// Cases are the probe's interactions, ordered cheapest-first so a model
// that fails the greeting does not pay for the rest.
//
// Two expectations, not one. A model that never calls tools passes the
// greeting and fails read-file; the rc7 model did the opposite. Probing
// only one direction grades half the defect.
var Cases = []Case{
	{
		Name:         "greeting",
		Prompt:       "hello",
		WantToolCall: false,
		Why: "The literal rc7 case: a bare greeting, with a full tool set in the request. " +
			"A compliant model answers in prose. qwen2.5-coder-14b emitted a JSON object " +
			"naming a tool that was never offered.",
	},
	{
		Name: "read-file",
		Prompt: "What is in the file /etc/hostname? Use the tools available to you to find out — " +
			"do not guess at the contents.",
		WantToolCall: true,
		Why: "Cannot be answered without a tool, so the model must emit a structured tool_use. " +
			"Catches the model that only ever talks, which the greeting case alone would pass.",
	},
	{
		Name: "search-then-edit",
		Prompt: "Find every Go file under internal/router that mentions quality_tier, then read the " +
			"first one. Start by using a tool to search.",
		WantToolCall: true,
		Why: "A two-step request over a larger tool set: the model has to choose between several " +
			"plausible tools (Glob, Grep, Read, Bash) rather than having exactly one obvious answer.",
	},
}

// BuildRequest assembles the fixture request for one case against one
// model. stream is false: the probe classifies a whole turn, and the
// non-streaming path is the one whose block structure Classify reads.
func BuildRequest(model string, c Case) (gateway.AnthropicRequest, error) {
	tools, err := fixtureTools()
	if err != nil {
		return gateway.AnthropicRequest{}, err
	}
	system, err := json.Marshal([]map[string]any{
		{"type": "text", "text": fixtureSystemPrompt},
	})
	if err != nil {
		return gateway.AnthropicRequest{}, fmt.Errorf("agentgrade: marshal system: %w", err)
	}
	msgs, err := fixtureMessages(c)
	if err != nil {
		return gateway.AnthropicRequest{}, err
	}
	if model == "" {
		model = fixtureModel
	}
	return gateway.AnthropicRequest{
		Model:     model,
		MaxTokens: 2048,
		System:    system,
		Tools:     tools,
		Messages:  msgs,
	}, nil
}

// fixtureMessages builds the conversation: the accumulated session
// context first (as a prior assistant/tool exchange, which is what a
// coding agent's context actually looks like mid-session), then the
// case's user turn.
func fixtureMessages(c Case) ([]gateway.AnthropicMessage, error) {
	userOpening, err := json.Marshal([]map[string]any{
		{"type": "text", "text": fixtureProjectContext},
	})
	if err != nil {
		return nil, fmt.Errorf("agentgrade: marshal context: %w", err)
	}
	assistantAck, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "Understood — I have the project context. What would you like me to do?"},
	})
	if err != nil {
		return nil, fmt.Errorf("agentgrade: marshal ack: %w", err)
	}
	prompt, err := json.Marshal([]map[string]any{{"type": "text", "text": c.Prompt}})
	if err != nil {
		return nil, fmt.Errorf("agentgrade: marshal prompt: %w", err)
	}
	return []gateway.AnthropicMessage{
		{Role: "user", Content: userOpening},
		{Role: "assistant", Content: assistantAck},
		{Role: "user", Content: prompt},
	}, nil
}

// ToolNames is the set of tools the fixture offers, for Classify's
// hallucination check.
func ToolNames() (map[string]bool, error) {
	tools, err := fixtureTools()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.Name] = true
	}
	return out, nil
}

func fixtureTools() ([]gateway.AnthropicTool, error) {
	out := make([]gateway.AnthropicTool, 0, len(fixtureToolSpecs))
	for _, s := range fixtureToolSpecs {
		schema, err := json.Marshal(s.Schema)
		if err != nil {
			return nil, fmt.Errorf("agentgrade: marshal schema for %s: %w", s.Name, err)
		}
		out = append(out, gateway.AnthropicTool{
			Name:        s.Name,
			Description: s.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

type toolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
}

// obj / arr / str are terse constructors so the schemas below read as
// schemas rather than as map literals.
func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string, enum ...string) map[string]any {
	m := map[string]any{"type": "string", "description": desc}
	if len(enum) > 0 {
		m["enum"] = enum
	}
	return m
}

func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func boolp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func arr(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}
