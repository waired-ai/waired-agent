// Package agentgrade measures whether a model can actually drive a
// coding agent's tool-call format (#322).
//
// The catalog's `capabilities` field and the picker's RequireCapability
// filter only encode "the chat template supports tools" — every bundled
// manifest advertises the same ["chat","tool_use","json_mode"] string.
// That is a fact about the TEMPLATE, and a model whose template renders
// tools correctly can still fail to comply with the format under a real
// coding-agent harness: qwen2.5-coder-14b at Q4_K_M emitted bare
// {"name":…,"arguments":…} objects as assistant TEXT and invented tools
// that were never in the request (waired-ai/waired#986, rc7 review).
// Ollama's parser extracts no tool_calls from that, the gateway
// correctly maps content to a text block, and the coding agent prints
// the JSON as prose.
//
// This package drives the real gateway surface with a request shaped
// like a coding agent's (many complex tool schemas + a large system
// prompt — see fixture.go) and classifies what comes back. It is the
// measurement behind the catalog's agent-grade verdicts; nothing here
// asserts a grade from reputation, generation, or context-window size,
// because none of those separate the observed cases (gpt-oss-20b has a
// 131072 window and complies; qwen3.5-0.8b has 262144 and cannot).
package agentgrade

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// Verdict is one probe case's outcome.
//
// The failure verdicts are deliberately distinct rather than a single
// "fail": they are different defects with different fixes, and lumping
// them together is the mistake #322 documents in the capabilities field
// (one string standing in for several independent questions). A model
// that answers in prose is unusable for a different reason than one
// that emits the right call in the wrong syntax.
type Verdict string

const (
	// VerdictPass: the case's expectation was met.
	VerdictPass Verdict = "pass"

	// VerdictUnstructuredToolCall: the model decided to call a tool and
	// serialised it as assistant TEXT instead of a structured call —
	// the rc7 symptom. The engine's parser saw no tool call, so the
	// agent gets JSON printed at the user.
	VerdictUnstructuredToolCall Verdict = "fail_unstructured_tool_call"

	// VerdictUnknownTool: the model named a tool that was not in the
	// request. Hallucination, not a syntax problem — no gateway-side
	// salvage parser can repair it, which is why #322 rejects one.
	VerdictUnknownTool Verdict = "fail_unknown_tool"

	// VerdictNoToolCall: the case required a tool call and the model
	// answered in prose without attempting one. It cannot drive an
	// agentic loop.
	VerdictNoToolCall Verdict = "fail_no_tool_call"

	// VerdictUnpromptedToolCall: the case expected plain text and the
	// model called a tool anyway. Weaker evidence than the others — a
	// real tool invoked over-eagerly is a usability problem, not a
	// format defect — so the grading policy treats it as a warning
	// unless the tool was also unknown (which classifies as
	// VerdictUnknownTool instead).
	VerdictUnpromptedToolCall Verdict = "warn_unprompted_tool_call"

	// VerdictMalformedToolCall: the model emitted tool-call syntax the
	// ENGINE could not parse, so the request failed upstream instead of
	// returning a turn. Measured: qwen3.5:4b-q4_K_M produced
	// `XML syntax error on line 14: element <function> closed by
	// </parameter>` and ollama answered 500.
	//
	// This is a model-quality failure, not an infrastructure one, and
	// getting that split right matters more than it looks: an engine
	// error is recorded as "unmeasured", and a model recorded as
	// unmeasured is a model that never gets retired. Emitting garbage
	// the serving engine rejects would then be the one way to dodge the
	// gate entirely.
	//
	// The attribution holds even if such a parse error were ever the
	// engine's fault rather than the model's: the user's experience is
	// that this model does not work on this engine, which is exactly
	// what a verdict is for. The record stores the engine version, so
	// an engine-side fix is re-measured rather than assumed.
	VerdictMalformedToolCall Verdict = "fail_malformed_tool_call"

	// VerdictError: the probe could not obtain an answer — transport
	// failure, an engine that is not running, a timeout, an undecodable
	// body. NOT a quality verdict.
	// waired-ai/waired-agent#203 records the cost of reporting an
	// upstream failure as a measurement result: the grading policy must
	// never turn a registry hiccup into a demoted model.
	VerdictError Verdict = "error"
)

// IsFailure reports whether v is a model-quality failure. Errors are
// not failures (see VerdictError) and warnings are not failures.
func (v Verdict) IsFailure() bool {
	switch v {
	case VerdictUnstructuredToolCall, VerdictUnknownTool, VerdictNoToolCall, VerdictMalformedToolCall:
		return true
	}
	return false
}

// engineParseFailureMarkers are substrings that identify an upstream
// error as "the engine could not parse the tool call the MODEL
// emitted", as opposed to any other 5xx.
//
// Deliberately narrow. Every entry names a parse of generated content;
// none of them can be produced by an engine that is merely down,
// loading, out of memory, or missing the model — those must keep
// classifying as VerdictError, or the split this list exists to make
// collapses in the wrong direction.
//
// Add to this list only from an observed run, and say which model
// produced it.
var engineParseFailureMarkers = []string{
	"XML syntax error",        // ollama, qwen3.5:4b-q4_K_M — measured 2026-08-01
	"error parsing tool call", // ollama tool-call parser
	"invalid tool call",
	"failed to parse tool",
	"unexpected end of JSON input", // truncated/instructured call in a tool arg
}

// IsEngineParseFailure reports whether an upstream error body shows the
// engine rejecting the model's own tool-call output.
func IsEngineParseFailure(body string) bool {
	for _, m := range engineParseFailureMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// Result is one case's classified outcome plus the evidence behind it,
// so a transcript shows WHY rather than only WHAT.
type Result struct {
	Case    string  `json:"case"`
	Verdict Verdict `json:"verdict"`
	// Detail is a one-line human explanation of the verdict.
	Detail string `json:"detail,omitempty"`
	// ToolsCalled lists the tool names of any structured tool_use
	// blocks, in order.
	ToolsCalled []string `json:"tools_called,omitempty"`
	// Evidence is the offending fragment (the bare JSON, the unknown
	// tool name), truncated. Empty on a pass.
	Evidence string `json:"evidence,omitempty"`
	// Text is the assistant's visible text, truncated — kept even on a
	// pass so a reviewer can read what the model actually said.
	Text string `json:"text,omitempty"`
	// StopReason is the turn's Anthropic stop_reason.
	StopReason string `json:"stop_reason,omitempty"`

	// Trials and FailedTrials count how often this case ran and how
	// often it came back a failure.
	//
	// The ratio, not just the fact of disagreement, is what a retirement
	// decision turns on: "emitted the wrong syntax on all three trials"
	// and "hit one engine parse error in three" are different models
	// with different answers, and a bare flaky flag collapses them.
	// Recorded so the grading POLICY can change without re-measuring the
	// catalog.
	Trials       int `json:"trials,omitempty"`
	FailedTrials int `json:"failed_trials,omitempty"`
}

// Case is one probe interaction: a prompt plus what a
// harness-compliant model must do with it.
type Case struct {
	// Name identifies the case in transcripts and verdict tables.
	Name string
	// Prompt is the user turn appended after the fixture's system
	// prompt and tool set.
	Prompt string
	// WantToolCall is true when the prompt cannot be answered without
	// invoking a tool, so a structured tool_use block is required.
	// False when the prompt is small talk that must be answered as
	// plain text.
	WantToolCall bool
	// Why documents what the case is probing, for the transcript.
	Why string
}

const evidenceMax = 400

// Classify turns one response into a verdict against the case's
// expectation. known is the set of tool names the request actually
// offered; a call to anything outside it is a hallucination.
func Classify(c Case, resp gateway.AnthropicResponse, known map[string]bool) Result {
	r := Result{Case: c.Name, StopReason: resp.StopReason}

	var text strings.Builder
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			r.ToolsCalled = append(r.ToolsCalled, b.Name)
		}
	}
	r.Text = truncate(text.String())

	// An unknown tool name outranks every other reading: the model did
	// not misformat a real call, it made one up. Checked before the
	// expectation split because it is wrong in both directions.
	for _, name := range r.ToolsCalled {
		if !known[name] {
			r.Verdict = VerdictUnknownTool
			r.Evidence = name
			r.Detail = fmt.Sprintf("called %q, which was not among the %d tools in the request", name, len(known))
			return r
		}
	}

	// Tool syntax that leaked into the assistant's TEXT. This is the
	// defect the whole package exists for, and it is invisible to the
	// engine: Ollama's parser found no tool call, so the gateway
	// faithfully mapped prose to a text block.
	if frag, name, ok := findUnstructuredToolCall(text.String()); ok {
		r.Verdict = VerdictUnstructuredToolCall
		r.Evidence = truncate(frag)
		switch {
		case name != "" && !known[name]:
			r.Detail = fmt.Sprintf("serialised a call to %q (not a tool in the request) as assistant text instead of a structured tool call", name)
		case name != "":
			r.Detail = fmt.Sprintf("serialised a call to %q as assistant text instead of a structured tool call", name)
		default:
			r.Detail = "emitted tool-call syntax as assistant text instead of a structured tool call"
		}
		return r
	}

	if c.WantToolCall {
		if len(r.ToolsCalled) == 0 {
			r.Verdict = VerdictNoToolCall
			r.Detail = "answered without attempting a tool call, so it cannot drive an agentic loop"
			return r
		}
		r.Verdict = VerdictPass
		r.Detail = fmt.Sprintf("emitted a structured tool_use for %s", strings.Join(r.ToolsCalled, ", "))
		return r
	}

	if len(r.ToolsCalled) > 0 {
		r.Verdict = VerdictUnpromptedToolCall
		r.Evidence = strings.Join(r.ToolsCalled, ", ")
		r.Detail = "called a tool for a prompt that needed none"
		return r
	}
	if strings.TrimSpace(text.String()) == "" {
		r.Verdict = VerdictNoToolCall
		r.Detail = "produced no visible content at all"
		return r
	}
	r.Verdict = VerdictPass
	r.Detail = "answered as plain text, with no tool syntax in the body"
	return r
}

// toolCallArgKeys are the argument-object keys a serialised tool call
// carries across the formats models actually emit: Anthropic's own
// wire name, OpenAI's, and the Hermes template's.
var toolCallArgKeys = []string{"arguments", "parameters", "input"}

// toolSyntaxMarkers are the tool-protocol delimiters that models emit
// as literal text when they do not comply with their own template. Any
// of them in the assistant's visible content means the engine's parser
// did not consume what the model produced, so no tool call was made and
// the user is looking at protocol scaffolding.
//
// The list is measured, not guessed: qwen2.5:0.5b answered a plain
// greeting with a correct sentence followed by
// `<tools>iface {{}} => {"main": true} </tools>` and a fragment of
// somebody else's task prompt. That response contained no
// name+arguments object at all, so a detector looking only for the rc7
// JSON shape graded it a pass — which is how a visibly broken model
// would have been recorded as compliant. Add a marker here whenever a
// run turns one up.
var toolSyntaxMarkers = []struct{ open, close string }{
	{"<tool_call>", "</tool_call>"},
	{"<tools>", "</tools>"},
	{"<function_call>", "</function_call>"},
	{"<|tool_call|>", ""},
	{"[TOOL_CALLS]", ""},
	{"<|python_tag|>", ""},
}

// findUnstructuredToolCall looks for tool-call syntax that ended up in
// the assistant's visible text. Two shapes, both observed:
//
//   - a chat template's own delimiters leaking verbatim (see
//     toolSyntaxMarkers), which means the engine's parser did not
//     consume them;
//   - a bare {"name":…,"arguments":{…}} object, with or without
//     surrounding prose or a code fence — the rc7 transcript.
//
// The JSON shape deliberately requires BOTH a string "name" and one of
// the argument keys. A model discussing JSON, or returning a
// {"name": …} record as a legitimate answer, must not read as a defect:
// the probe's prompts are chosen so no correct answer contains that
// pair, or any of the markers.
func findUnstructuredToolCall(text string) (fragment, name string, found bool) {
	for _, m := range toolSyntaxMarkers {
		i := strings.Index(text, m.open)
		if i < 0 {
			continue
		}
		frag := text[i:]
		if m.close != "" {
			if j := strings.Index(frag, m.close); j >= 0 {
				frag = frag[:j+len(m.close)]
			}
		}
		n, _ := toolCallName(frag)
		return frag, n, true
	}
	for _, obj := range jsonObjects(text) {
		if n, ok := toolCallName(obj); ok {
			return obj, n, true
		}
	}
	return "", "", false
}

// toolCallName reports whether s contains a JSON object with a string
// "name" and an argument object, returning the name.
func toolCallName(s string) (string, bool) {
	for _, obj := range jsonObjects(s) {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(obj), &m) != nil {
			continue
		}
		raw, ok := m["name"]
		if !ok {
			continue
		}
		var name string
		if json.Unmarshal(raw, &name) != nil || name == "" {
			continue
		}
		for _, k := range toolCallArgKeys {
			if _, ok := m[k]; ok {
				return name, true
			}
		}
	}
	return "", false
}

// jsonObjects returns the balanced {...} spans in s that parse as JSON
// objects, outermost first. It brace-scans rather than regexing because
// the objects are nested and frequently wrapped in prose or a fence.
//
// Braces inside JSON string literals are skipped, so a description
// containing "{" does not desynchronise the scan.
func jsonObjects(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		end, ok := matchBrace(s, i)
		if !ok {
			// Unbalanced from here on: no later opener can close either.
			break
		}
		candidate := s[i : end+1]
		if json.Valid([]byte(candidate)) {
			out = append(out, candidate)
			i = end // outermost only; nested objects come along inside
			continue
		}
	}
	return out
}

// matchBrace returns the index of the '}' closing the '{' at start.
func matchBrace(s string, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func truncate(s string) string {
	s = strings.TrimFunc(s, unicode.IsSpace)
	if len(s) <= evidenceMax {
		return s
	}
	return s[:evidenceMax] + "…"
}
