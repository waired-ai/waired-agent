package agentgrade

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

func known(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func textResp(s string) gateway.AnthropicResponse {
	return gateway.AnthropicResponse{
		Content:    []gateway.AnthropicContentBlock{{Type: "text", Text: s}},
		StopReason: "end_turn",
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
