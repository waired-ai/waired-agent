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
	"math"
	"slices"
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

	// VerdictInvalidToolArguments: the model emitted a structured call
	// to a tool that WAS offered, carrying arguments the schema it was
	// shown does not admit — a required property missing, a value of the
	// wrong JSON type, an undeclared property where the schema closed
	// the object.
	//
	// This exists because until it did, Classify read only the tool
	// NAME: a call to Grep with no `pattern` was graded a pass. The
	// question the package asks is whether a coding agent can DRIVE the
	// model, and a call the agent cannot execute answers that as
	// squarely as one it cannot parse.
	//
	// A warning rather than a failure, deliberately and provisionally.
	// Every stored catalog verdict was measured before this check
	// existed, so promoting it straight to a failure would re-grade the
	// file against a rule it never saw. Its measured rate at
	// introduction is zero — 144 turns of qwen3.5:9b-q4_K_M over both
	// transports — which means this closes a blind spot rather than
	// repairing an observed defect, and the rate that would justify
	// promoting it is not yet known. Promote from #322, once a sweep has
	// run with the check in place.
	VerdictInvalidToolArguments Verdict = "warn_invalid_tool_arguments"

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

// IsEngineParseFailure reports whether an upstream error body shows the
// engine rejecting the model's own tool-call output.
//
// The list behind it lives in internal/gateway now: the gateway retries
// on this condition (#442), so it has to recognise it, and one list read
// by both keeps the probe from grading a failure the product silently
// repaired. The dependency only points this way — agentgrade already
// imports gateway, and gateway must not import a measurement package.
func IsEngineParseFailure(body string) bool {
	return gateway.IsEngineParseFailure(body)
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
// expectation. offered maps each tool name the request actually carried
// to the input_schema it carried: a call to a name outside the map is a
// hallucination, and a call whose arguments the mapped schema rejects is
// one the coding agent cannot execute.
func Classify(c Case, resp gateway.AnthropicResponse, offered map[string]json.RawMessage) Result {
	r := Result{Case: c.Name, StopReason: resp.StopReason}

	var text strings.Builder
	var calls []gateway.AnthropicContentBlock
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			r.ToolsCalled = append(r.ToolsCalled, b.Name)
			calls = append(calls, b)
		}
	}
	r.Text = truncate(text.String())

	// An unknown tool name outranks every other reading: the model did
	// not misformat a real call, it made one up. Checked before the
	// expectation split because it is wrong in both directions.
	for _, name := range r.ToolsCalled {
		if _, ok := offered[name]; !ok {
			r.Verdict = VerdictUnknownTool
			r.Evidence = name
			r.Detail = fmt.Sprintf("called %q, which was not among the %d tools in the request", name, len(offered))
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
		case name != "" && !offeredHas(offered, name):
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
		// The call exists and names a real tool. Whether the agent can
		// RUN it is a separate question, and one the name alone does not
		// answer.
		for _, b := range calls {
			if why := checkToolArguments(b.Name, b.Input, offered[b.Name]); why != "" {
				r.Verdict = VerdictInvalidToolArguments
				r.Evidence = truncate(string(b.Input))
				r.Detail = "emitted a structured tool_use the schema does not admit: " + why
				return r
			}
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

func offeredHas(offered map[string]json.RawMessage, name string) bool {
	_, ok := offered[name]
	return ok
}

// checkToolArguments reports how input fails the schema the tool was
// offered with, or "" when it satisfies it.
//
// Deliberately shallow — required properties, the declared type of each
// present property, and undeclared properties only where the schema
// itself set additionalProperties:false. It does not recurse into nested
// objects, because the failures worth grading are at the top level (a
// missing `pattern`, a string where an array of strings belongs) and a
// deep validator would put this package in the business of implementing
// JSON Schema.
//
// Every check follows something the schema DECLARES. Nothing here
// invents a stricter contract than the model was shown: an undeclared
// property is a defect when the schema closed the object and permitted
// otherwise, which is the same rule the tool on the other end applies.
//
// An empty or undecodable schema returns "" rather than a violation.
// The schema is ours; a broken one is our defect, and reporting it
// against the model would be the exact mis-attribution the malformed /
// error split exists to prevent.
func checkToolArguments(name string, input, schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	var s struct {
		Properties           map[string]struct{ Type json.RawMessage } `json:"properties"`
		Required             []string                                  `json:"required"`
		AdditionalProperties *bool                                     `json:"additionalProperties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return ""
	}

	args := map[string]json.RawMessage{}
	if len(input) > 0 {
		if json.Unmarshal(input, &args) != nil {
			return fmt.Sprintf("%s: arguments are not a JSON object", name)
		}
	}
	for _, req := range s.Required {
		if _, ok := args[req]; !ok {
			return fmt.Sprintf("%s: required argument %q is missing", name, req)
		}
	}
	// Sorted so the reported violation does not depend on map order: a
	// verdict that changes between identical runs is not a measurement.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p, declared := s.Properties[k]
		if !declared {
			if s.AdditionalProperties != nil && !*s.AdditionalProperties {
				return fmt.Sprintf("%s: argument %q is not declared and the schema forbids extras", name, k)
			}
			continue
		}
		got := jsonTypeOf(args[k])
		if want := declaredTypes(p.Type); len(want) > 0 && got != "" && !typeAdmits(want, got) {
			return fmt.Sprintf("%s: argument %q is %s, the schema declares %s",
				name, k, got, strings.Join(want, " or "))
		}
	}
	return ""
}

// typeAdmits reports whether the observed JSON type satisfies any type
// the schema declared.
//
// An integral number satisfies "number" as well as "integer": JSON has
// a single numeric type, so a model that writes 3 where the schema says
// number is indistinguishable on the wire from one that writes 3.0, and
// failing it would report a correct call as broken.
func typeAdmits(want []string, got string) bool {
	for _, w := range want {
		if w == got || (w == "number" && got == "integer") {
			return true
		}
	}
	return false
}

// declaredTypes reads a schema's `type`, which JSON Schema allows to be
// either one name or a list of them.
func declaredTypes(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// jsonTypeOf names the JSON type of an encoded value, in the vocabulary
// a schema's `type` uses. An integral number reports as both "integer"
// and "number" via typeAdmits; here it reports the wire type.
func jsonTypeOf(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return ""
	}
	switch t[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return ""
	}
	if f == math.Trunc(f) {
		return "integer"
	}
	return "number"
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
// orphanFragmentBytes bounds the evidence kept for a close delimiter
// that has no opener. Enough to carry the leaked call and the prose
// around it, short enough that the report stays a table.
const orphanFragmentBytes = 400

var toolSyntaxMarkers = []struct{ open, close string }{
	{"<tool_call>", "</tool_call>"},
	{"<tools>", "</tools>"},
	{"<function_call>", "</function_call>"},
	{"<function=", "</function>"}, // measured: qwen3-coder:30b-a3b
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

	// The same defect seen from the other side: a CLOSE delimiter with
	// no opener. The engine's parser recognised the opening marker and
	// consumed it, then failed on the body and passed the remainder
	// through as visible text — so the one delimiter left in the reply
	// is the closer. qwen3-coder:30b-a3b does exactly this (ollama eats
	// the "<tool_call>" and emits "<function=Bash>…</function>
	// </tool_call>"), and looking only for openers grades it as "no tool
	// call attempted", which is the opposite of what happened.
	//
	// This also closes the pass-flip: a model that returns a valid
	// structured call AND leaks a stray closer is not compliant, and
	// before this it read as one.
	for _, m := range toolSyntaxMarkers {
		if m.close == "" {
			continue
		}
		j := strings.Index(text, m.close)
		if j < 0 {
			continue
		}
		frag := text[:j+len(m.close)]
		// Keep the tail, not the head: without an opener the fragment
		// starts at the beginning of the reply, and the report truncates
		// evidence from the front — so an unbounded fragment would show
		// prose and hide the delimiter that is the whole finding.
		if len(frag) > orphanFragmentBytes {
			frag = "…" + frag[len(frag)-orphanFragmentBytes:]
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
