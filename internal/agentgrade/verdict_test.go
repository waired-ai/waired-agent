package agentgrade

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// known offers the named tools with no schema attached, so Classify's
// argument-conformance check has nothing to assert and these cases stay
// about the tool NAME and the call's syntax. Cases that are about the
// arguments use offering().
func known(names ...string) map[string]json.RawMessage {
	m := make(map[string]json.RawMessage, len(names))
	for _, n := range names {
		m[n] = nil
	}
	return m
}

// offering builds a one-tool offer whose schema is written inline, so a
// conformance case reads as the pair it is: this schema, that call.
func offering(name, schema string) map[string]json.RawMessage {
	return map[string]json.RawMessage{name: json.RawMessage(schema)}
}

func textResp(s string) gateway.AnthropicResponse {
	return gateway.AnthropicResponse{
		Content:    []gateway.AnthropicContentBlock{{Type: "text", Text: s}},
		StopReason: "end_turn",
	}
}

// toolAndTextResp is a turn that carries BOTH a structured tool call and
// visible text — the shape an engine produces when it parsed part of
// what the model emitted and passed the rest through.
func toolAndTextResp(name, args, text string) gateway.AnthropicResponse {
	return gateway.AnthropicResponse{
		Content: []gateway.AnthropicContentBlock{
			{Type: "text", Text: text},
			{Type: "tool_use", ID: "toolu_1", Name: name, Input: json.RawMessage(args)},
		},
		StopReason: "tool_use",
	}
}

func toolResp(name, args string) gateway.AnthropicResponse {
	return gateway.AnthropicResponse{
		Content: []gateway.AnthropicContentBlock{
			{Type: "tool_use", ID: "toolu_1", Name: name, Input: json.RawMessage(args)},
		},
		StopReason: "tool_use",
	}
}

// The rc7 transcript, verbatim from the review (waired-ai/waired#986
// comment-3, reproduced in waired-ai/waired-agent#322): a plain "hello"
// produced a bare JSON object naming a tool that was never offered.
//
// This is the case the whole package has to detect. If the classifier
// stops recognising it, the probe is worthless — it would grade the
// model that started all this as compliant.
const rc7Transcript = `{
    "name": "SendMessage",
    "arguments": {
      "to": "main",
      "summary": "greeting",
      "message": "Hello! How can I assist you today?"
    }
  }`

// The qwen3-coder transcript, verbatim from a 12-trial run against the
// real gateway (ollama 0.31.1, qwen3-coder:30b-a3b-q4_K_M, 2026-08-02).
//
// Kept alongside rc7Transcript because it is the second measured way a
// model fails this probe, and the two look nothing alike: rc7 emitted a
// bare JSON object, this one emits a template's XML with the opener
// already eaten by the engine's parser.
const qwen3CoderXMLTranscript = "I'll check the contents of `/etc/hostname` using a shell command.\n\n" +
	"<function=Bash>\n" +
	"<parameter=command>\n" +
	"cat /etc/hostname\n" +
	"</parameter>\n" +
	"<parameter=description>\n" +
	"Read the hostname file\n" +
	"</parameter>\n" +
	"</function>\n" +
	"</tool_call>"

func TestClassify_qwen3CoderXMLTranscript(t *testing.T) {
	needsTool := Case{Name: "read-file", Prompt: "read /etc/hostname", WantToolCall: true}
	got := Classify(needsTool, textResp(qwen3CoderXMLTranscript), known("Read", "Write", "Bash", "Glob", "Grep"))

	if got.Verdict != VerdictUnstructuredToolCall {
		t.Fatalf("verdict = %q, want %q (detail=%q)", got.Verdict, VerdictUnstructuredToolCall, got.Detail)
	}
	// The evidence has to carry the call itself. A maintainer reading
	// the report decides "can this model drive an agent" from this
	// string, and prose alone would not answer it.
	if !strings.Contains(got.Evidence, "<function=Bash>") {
		t.Errorf("evidence should carry the leaked call, got %q", got.Evidence)
	}
}

func TestClassify_rc7GreetingTranscript(t *testing.T) {
	greeting := Case{Name: "greeting", Prompt: "hello", WantToolCall: false}
	// The offered set is a realistic coding-agent one; SendMessage is
	// deliberately absent, as it was in the rc7 request.
	got := Classify(greeting, textResp(rc7Transcript), known("Read", "Write", "Bash", "Glob", "Grep"))

	if got.Verdict != VerdictUnstructuredToolCall {
		t.Fatalf("verdict = %q, want %q (detail=%q)", got.Verdict, VerdictUnstructuredToolCall, got.Detail)
	}
	if !got.Verdict.IsFailure() {
		t.Error("the rc7 transcript must classify as a failure")
	}
	if !strings.Contains(got.Detail, "SendMessage") {
		t.Errorf("detail should name the tool it tried to call, got %q", got.Detail)
	}
	if !strings.Contains(got.Evidence, "SendMessage") {
		t.Errorf("evidence should carry the offending fragment, got %q", got.Evidence)
	}
}

func TestClassify(t *testing.T) {
	offered := known("Read", "Write", "Bash", "Glob", "Grep")
	greeting := Case{Name: "greeting", Prompt: "hello", WantToolCall: false}
	needsTool := Case{Name: "read-file", Prompt: "read /etc/hostname", WantToolCall: true}

	tests := []struct {
		name string
		c    Case
		resp gateway.AnthropicResponse
		want Verdict
	}{
		{
			name: "greeting answered as text passes",
			c:    greeting,
			resp: textResp("Hello! How can I help you with your code today?"),
			want: VerdictPass,
		},
		{
			name: "structured call to an offered tool passes",
			c:    needsTool,
			resp: toolResp("Read", `{"file_path":"/etc/hostname"}`),
			want: VerdictPass,
		},
		{
			name: "structured call to a tool that was never offered is hallucination",
			c:    needsTool,
			resp: toolResp("mkdir", `{"path":"/tmp/x"}`),
			want: VerdictUnknownTool,
		},
		{
			name: "bare JSON in text when a tool was required",
			c:    needsTool,
			resp: textResp(`{"name":"Read","arguments":{"file_path":"/etc/hostname"}}`),
			want: VerdictUnstructuredToolCall,
		},
		{
			name: "template wrapper leaking as text",
			c:    needsTool,
			resp: textResp("<tool_call>{\"name\": \"Read\", \"arguments\": {\"file_path\": \"/etc/hostname\"}}</tool_call>"),
			want: VerdictUnstructuredToolCall,
		},
		{
			// Measured: qwen2.5:0.5b answered a greeting with a correct
			// sentence and then this. There is no name+arguments object
			// anywhere in it, so the JSON detector alone graded the turn
			// a pass — a visibly broken model recorded as compliant.
			name: "bare template delimiters with no JSON object at all",
			c:    greeting,
			resp: textResp("Hello! Is there anything specific you'd like help with today?\n\n" +
				"# Task: Select the largest bundled model that fits the host.\n\n" +
				"<tools>iface {{}} => {\"main\": true} </tools>"),
			want: VerdictUnstructuredToolCall,
		},
		{
			// Measured: qwen3-coder:30b-a3b, 2 trials in 12. The engine
			// recognised and consumed the opening "<tool_call>" and then
			// failed on the body, so the reply carries the model's own
			// XML dialect and an orphaned closer. Looking only for
			// OPENING delimiters graded this "no tool call attempted" —
			// the opposite of what happened.
			name: "qwen3-coder XML call dialect leaking as text",
			c:    needsTool,
			resp: textResp(qwen3CoderXMLTranscript),
			want: VerdictUnstructuredToolCall,
		},
		{
			// The pass-flip this closes. A structured call is present and
			// correct, so every other check would pass the turn — but a
			// closer in the visible text means the engine parsed only
			// part of what the model emitted, and a coding agent gets the
			// rest as prose. Pins a product contract, not today's
			// behaviour: leaked delimiters are a failure even when a
			// well-formed call sits beside them (same rule as the
			// qwen2.5:0.5b case above).
			name: "orphaned closer beside a valid structured call is not a pass",
			c:    needsTool,
			resp: toolAndTextResp("Read", `{"file_path":"/etc/hostname"}`,
				"I'll read that for you.\n</tool_call>"),
			want: VerdictUnstructuredToolCall,
		},
		{
			name: "llama-style tool tag leaking as text",
			c:    needsTool,
			resp: textResp("[TOOL_CALLS]Read"),
			want: VerdictUnstructuredToolCall,
		},
		{
			name: "tool-call JSON wrapped in prose and a code fence",
			c:    needsTool,
			resp: textResp("Sure, I'll read that file.\n\n```json\n{\"name\": \"Read\", \"parameters\": {\"file_path\": \"/etc/hostname\"}}\n```\n"),
			want: VerdictUnstructuredToolCall,
		},
		{
			name: "prose answer where a tool call was required",
			c:    needsTool,
			resp: textResp("The file /etc/hostname usually contains the machine's hostname."),
			want: VerdictNoToolCall,
		},
		{
			name: "tool call on a bare greeting is a warning, not a failure",
			c:    greeting,
			resp: toolResp("Read", `{"file_path":"/etc/hostname"}`),
			want: VerdictUnpromptedToolCall,
		},
		{
			name: "empty turn",
			c:    greeting,
			resp: gateway.AnthropicResponse{Content: []gateway.AnthropicContentBlock{}},
			want: VerdictNoToolCall,
		},
		{
			name: "thinking block before a structured call still passes",
			c:    needsTool,
			resp: gateway.AnthropicResponse{Content: []gateway.AnthropicContentBlock{
				{Type: "thinking", Thinking: "I should call Read with the path."},
				{Type: "tool_use", ID: "toolu_1", Name: "Read", Input: json.RawMessage(`{"file_path":"/etc/hostname"}`)},
			}, StopReason: "tool_use"},
			want: VerdictPass,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.c, tt.resp, offered)
			if got.Verdict != tt.want {
				t.Fatalf("verdict = %q, want %q (detail=%q evidence=%q)",
					got.Verdict, tt.want, got.Detail, got.Evidence)
			}
		})
	}
}

// A model that merely TALKS about JSON, or legitimately answers with a
// record that happens to have a "name" field, must not read as a
// defect. The detector requires a string name AND an argument object
// together precisely so these stay clean — a false positive here would
// demote a compliant model.
func TestClassify_noFalsePositiveOnOrdinaryJSON(t *testing.T) {
	greeting := Case{Name: "greeting", Prompt: "hello", WantToolCall: false}
	offered := known("Read", "Write")

	for _, body := range []string{
		`Hello! Here is an example config: {"name": "waired", "version": 3}.`,
		`Sure — a tool call looks like a JSON object with a name and its arguments.`,
		`{"arguments": ["--verbose"], "exit_code": 0}`,
		"Hello! How can I help?",
	} {
		got := Classify(greeting, textResp(body), offered)
		if got.Verdict != VerdictPass {
			t.Errorf("body %q: verdict = %q (detail=%q), want pass", body, got.Verdict, got.Detail)
		}
	}
}

func TestFindUnstructuredToolCall_bracesInsideStrings(t *testing.T) {
	// A '{' inside a JSON string literal must not desynchronise the
	// brace scan — otherwise the detector silently stops finding calls
	// in any response whose arguments mention a brace.
	body := `{"name":"Bash","arguments":{"command":"echo \"{not json}\"","description":"print {}"}}`
	frag, name, ok := findUnstructuredToolCall(body)
	if !ok {
		t.Fatal("expected to find the tool call")
	}
	if name != "Bash" {
		t.Errorf("name = %q, want Bash", name)
	}
	if !strings.HasSuffix(frag, "}") {
		t.Errorf("fragment should be the balanced object, got %q", frag)
	}
}

func TestVerdictIsFailure(t *testing.T) {
	// An infra error must never read as a model-quality failure
	// (waired-ai/waired-agent#203: a failed benchmark reported as a
	// verdict de-rated the node indefinitely).
	if VerdictError.IsFailure() {
		t.Error("VerdictError must not count as a model-quality failure")
	}
	if VerdictUnpromptedToolCall.IsFailure() {
		t.Error("a warning must not count as a failure")
	}
	if VerdictPass.IsFailure() {
		t.Error("pass is not a failure")
	}
	for _, v := range []Verdict{VerdictUnstructuredToolCall, VerdictUnknownTool, VerdictNoToolCall} {
		if !v.IsFailure() {
			t.Errorf("%q must count as a failure", v)
		}
	}
}

// A structured call to a real tool is only half the question: the coding
// agent still has to be able to RUN it. Before
// VerdictInvalidToolArguments, Classify read the tool NAME and nothing
// else, so a Grep with no pattern graded a pass.
//
// Every case here pairs a schema with a call, because the verdict is a
// statement about the pair — the same arguments are correct or broken
// depending on what the model was shown.
func TestClassifyArgumentConformance(t *testing.T) {
	needsTool := Case{Name: "read-file", Prompt: "read /etc/hostname", WantToolCall: true}

	// The shape fixtureTools() emits: a closed object with required
	// properties, which is what the model is actually offered.
	const grepSchema = `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string"},
			"path":    {"type": "string"},
			"lines":   {"type": "number"},
			"globs":   {"type": "array"}
		},
		"required": ["pattern"],
		"additionalProperties": false
	}`

	tests := []struct {
		name    string
		schema  string
		args    string
		want    Verdict
		wantWhy string // substring of Detail, so the message stays useful
	}{
		{
			name:   "arguments satisfying the schema pass",
			schema: grepSchema,
			args:   `{"pattern":"quality_tier","path":"internal/router"}`,
			want:   VerdictPass,
		},
		{
			name:    "a missing required argument is not executable",
			schema:  grepSchema,
			args:    `{"path":"internal/router"}`,
			want:    VerdictInvalidToolArguments,
			wantWhy: `required argument "pattern" is missing`,
		},
		{
			name:    "no arguments at all still misses the required one",
			schema:  grepSchema,
			args:    ``,
			want:    VerdictInvalidToolArguments,
			wantWhy: `required argument "pattern" is missing`,
		},
		{
			name:    "a string where the schema declares an array",
			schema:  grepSchema,
			args:    `{"pattern":"x","globs":"*.go"}`,
			want:    VerdictInvalidToolArguments,
			wantWhy: `"globs" is string, the schema declares array`,
		},
		{
			// The fixture's obj() sets additionalProperties:false, so an
			// undeclared key is a violation the SCHEMA declares — not a
			// stricter contract invented here.
			name:    "an undeclared argument where the schema closed the object",
			schema:  grepSchema,
			args:    `{"pattern":"x","recursive":true}`,
			want:    VerdictInvalidToolArguments,
			wantWhy: `"recursive" is not declared`,
		},
		{
			name:   "an undeclared argument where the schema permits extras",
			schema: `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`,
			args:   `{"pattern":"x","recursive":true}`,
			want:   VerdictPass,
		},
		{
			// JSON has one numeric type: 3 and 3.0 are the same bytes, so
			// rejecting 3 for a "number" would fail a correct call.
			name:   "an integral number satisfies a number property",
			schema: grepSchema,
			args:   `{"pattern":"x","lines":3}`,
			want:   VerdictPass,
		},
		{
			name:   "a union type admits either member",
			schema: `{"type":"object","properties":{"path":{"type":["string","null"]}}}`,
			args:   `{"path":null}`,
			want:   VerdictPass,
		},
		{
			// The schema is ours. A broken one is our defect, and charging
			// it to the model is the mis-attribution the malformed/error
			// split exists to prevent.
			name:   "an unusable schema is not the model's failure",
			schema: `{{ not json`,
			args:   `{"anything":1}`,
			want:   VerdictPass,
		},
		{
			name:    "arguments that are not an object at all",
			schema:  grepSchema,
			args:    `["quality_tier"]`,
			want:    VerdictInvalidToolArguments,
			wantWhy: "arguments are not a JSON object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(needsTool, toolResp("Grep", tt.args), offering("Grep", tt.schema))
			if got.Verdict != tt.want {
				t.Fatalf("verdict = %s, want %s (detail: %s)", got.Verdict, tt.want, got.Detail)
			}
			if tt.wantWhy != "" && !strings.Contains(got.Detail, tt.wantWhy) {
				t.Errorf("detail = %q, want it to name %q", got.Detail, tt.wantWhy)
			}
		})
	}
}

// INVERTED by #483. This test previously asserted the opposite — that
// the class must NOT be a failure — because every stored verdict predated
// the check and a promotion would have re-graded the file against a rule
// it never saw. #479 re-measured the catalog with the check in place and
// counted every class, which removed both halves of that objection: the
// records were taken under the rule, and the tally says what promoting it
// costs (nobody reaches the retirement line).
//
// The severity assertions are unchanged, and that is the point of keeping
// them here: promotion moved IsFailure and deliberately did not move the
// ladder. A failure it is; the mildest one it remains.
func TestInvalidToolArgumentsIsAFailure(t *testing.T) {
	if !VerdictInvalidToolArguments.IsFailure() {
		t.Error("a call the agent cannot execute must count as a failure (#483)")
	}
	if !strings.HasPrefix(string(VerdictInvalidToolArguments), "fail_") {
		t.Errorf("a failing class must be spelled fail_*, got %q — the string is stored, "+
			"and a record reading warn_* with a nonzero failed count cannot be read",
			VerdictInvalidToolArguments)
	}
	if Severity(VerdictInvalidToolArguments) >= Severity(VerdictNoToolCall) {
		t.Error("bad arguments should rank below no call at all: the model at least reached " +
			"for the right tool")
	}
	if Severity(VerdictInvalidToolArguments) <= Severity(VerdictUnpromptedToolCall) {
		t.Error("an unexecutable call should outrank a merely unnecessary one")
	}
}

// A stored verdict written before the #483 rename still has to load as
// the class it names. An unknown class is silently the most harmless
// thing in the package — IsFailure says no, Severity returns the pass
// rank — so a renamed failure that is not mapped stops counting without
// any error.
func TestCanonicalVerdictMapsTheOldSpelling(t *testing.T) {
	got := CanonicalVerdict("warn_invalid_tool_arguments")
	if got != VerdictInvalidToolArguments {
		t.Fatalf("CanonicalVerdict(old) = %q, want %q", got, VerdictInvalidToolArguments)
	}
	if !got.IsFailure() || Severity(got) == Severity(VerdictPass) {
		t.Errorf("the mapped verdict did not survive as a failure: IsFailure=%v severity=%d",
			got.IsFailure(), Severity(got))
	}
	// Everything current passes through untouched, including the class
	// that is still a warning.
	for _, v := range []Verdict{
		VerdictPass, VerdictUnpromptedToolCall, VerdictInvalidToolArguments,
		VerdictNoToolCall, VerdictUnknownTool, VerdictUnstructuredToolCall,
		VerdictMalformedToolCall, VerdictError,
	} {
		if CanonicalVerdict(v) != v {
			t.Errorf("CanonicalVerdict(%q) = %q, want it unchanged", v, CanonicalVerdict(v))
		}
	}
}

// A hallucinated name outranks bad arguments: no parser and no schema
// repairs a tool that does not exist, and reporting the arguments would
// bury the larger defect.
func TestUnknownToolOutranksInvalidArguments(t *testing.T) {
	needsTool := Case{Name: "read-file", Prompt: "read /etc/hostname", WantToolCall: true}
	got := Classify(needsTool, toolResp("Systemctl", `{}`), offering("Grep", `{"required":["pattern"]}`))
	if got.Verdict != VerdictUnknownTool {
		t.Errorf("verdict = %s, want %s", got.Verdict, VerdictUnknownTool)
	}
}
