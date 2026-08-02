package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// The transcripts below are verbatim from runs against the real gateway
// (ollama 0.31.1, 2026-08-01/02), recorded as fixtures in
// internal/agentgrade/verdict_test.go. They are the evidence #409 is
// built on: in every one the model chose the right tool with the right
// arguments and only the serialisation was lost.

// Measured: qwen3-coder:30b-a3b-q4_K_M, 8/24 trials. Note the missing
// OPENING <tool_call> (ollama's parser consumed it before failing on the
// body) and the orphaned closer left behind.
const xmlFunctionTranscript = "I'll check the contents of `/etc/hostname` using a shell command.\n\n" +
	"<function=Bash>\n" +
	"<parameter=command>\n" +
	"cat /etc/hostname\n" +
	"</parameter>\n" +
	"<parameter=description>\n" +
	"Read the hostname file\n" +
	"</parameter>\n" +
	"</function>\n" +
	"</tool_call>"

// Measured: qwen2.5-coder 0.5b/3b/7b/14b, 24/24 trials — every size,
// every trial, including the compiled-in BundledModelID.
const fencedJSONTranscript = "I'll read that file for you.\n\n" +
	"```json\n" +
	`{"name": "Read", "arguments": {"file_path": "/etc/hostname"}}` + "\n" +
	"```"

// Measured: granite4:350m, 5/12 trials. The name sits OUTSIDE any JSON
// object, and the template leaves an empty slot object behind.
const delimitedTranscript = `[TOOL_CALLS]Read{"file_path":"/etc/hostname"}{}`

// readTools is the offered set these transcripts were measured against,
// trimmed to what the assertions need. Types matter: `limit` is declared
// numeric, which is what coerceBySchema keys off.
func readTools() []AnthropicTool {
	return []AnthropicTool{
		{
			Name:        "Read",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"limit":{"type":"number"},"raw":{"type":"boolean"}}}`),
		},
		{
			Name:        "Bash",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"},"timeout":{"type":"number"}}}`),
		},
		{
			Name:        "Grep",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}}}`),
		},
	}
}

// Product contract (#409): each measured dialect becomes a structured
// call, and the fragment is removed from the text so the user never sees
// the scaffolding — including the wrapper delimiters and code fence
// around it, which are not part of the call but are not prose either.
func TestRecoverToolCall_measuredDialects(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantTool  string
		wantShape string
		wantArgs  map[string]any
		wantText  string
	}{
		{
			name:      "qwen3-coder XML with an orphaned closer",
			text:      xmlFunctionTranscript,
			wantTool:  "Bash",
			wantShape: toolRecoveryXML,
			wantArgs:  map[string]any{"command": "cat /etc/hostname", "description": "Read the hostname file"},
			wantText:  "I'll check the contents of `/etc/hostname` using a shell command.",
		},
		{
			name:      "qwen2.5-coder fenced JSON object",
			text:      fencedJSONTranscript,
			wantTool:  "Read",
			wantShape: toolRecoveryJSON,
			wantArgs:  map[string]any{"file_path": "/etc/hostname"},
			wantText:  "I'll read that file for you.",
		},
		{
			name:      "granite4 [TOOL_CALLS] delimiter",
			text:      delimitedTranscript,
			wantTool:  "Read",
			wantShape: toolRecoveryDelimited,
			wantArgs:  map[string]any{"file_path": "/etc/hostname"},
			wantText:  "",
		},
		{
			name:      "XML wrapped in a complete tool_call pair",
			text:      "<tool_call>\n<function=Bash>\n<parameter=command>\nls\n</parameter>\n</function>\n</tool_call>",
			wantTool:  "Bash",
			wantShape: toolRecoveryXML,
			wantArgs:  map[string]any{"command": "ls"},
			wantText:  "",
		},
		{
			// Measured: qwen2.5-coder:14b-instruct-q4_K_M, the one case
			// still failing after the first cut of this change. OpenAI's
			// own field name for the tool, so a name-only scan reads it
			// as ordinary JSON and leaves the call in the text.
			name: "fenced JSON naming the tool under \"function\"",
			text: "I'll search for that.\n\n```json\n" +
				`{"function": "Grep", "arguments": {"pattern": "quality_tier"}}` + "\n```",
			wantTool:  "Grep",
			wantShape: toolRecoveryJSON,
			wantArgs:  map[string]any{"pattern": "quality_tier"},
			wantText:  "I'll search for that.",
		},
		{
			name:      "bare JSON using the parameters key",
			text:      `Sure. {"name":"Read","parameters":{"file_path":"/etc/hostname"}} done.`,
			wantTool:  "Read",
			wantShape: toolRecoveryJSON,
			wantArgs:  map[string]any{"file_path": "/etc/hostname"},
			wantText:  "Sure.  done.",
		},
	}

	offered := newOfferedTools(readTools())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ok := recoverToolCall(tt.text, offered)
			if !ok {
				t.Fatalf("no call recovered from:\n%s", tt.text)
			}
			if c.Name != tt.wantTool {
				t.Errorf("tool = %q, want %q", c.Name, tt.wantTool)
			}
			if c.Shape != tt.wantShape {
				t.Errorf("shape = %q, want %q", c.Shape, tt.wantShape)
			}
			var got map[string]any
			if err := json.Unmarshal(c.Input, &got); err != nil {
				t.Fatalf("recovered input is not a JSON object: %v (%s)", err, c.Input)
			}
			if len(got) != len(tt.wantArgs) {
				t.Errorf("args = %v, want %v", got, tt.wantArgs)
			}
			for k, want := range tt.wantArgs {
				if got[k] != want {
					t.Errorf("args[%q] = %#v, want %#v", k, got[k], want)
				}
			}
			if left := stripFragment(tt.text, c); left != tt.wantText {
				t.Errorf("remaining text = %q, want %q", left, tt.wantText)
			}
		})
	}
}

// Product contract, and the guard the issue is emphatic about: a name
// the request never offered is a HALLUCINATION (#322's rc7 transcript),
// not a serialisation defect. No parser can repair it, and converting it
// would turn a visible failure into an invisible one — the model would
// appear to call a tool that does not exist.
func TestRecoverToolCall_unofferedNameIsNotConverted(t *testing.T) {
	// Verbatim rc7: a plain "hello" produced a call to a tool that was
	// never in the request.
	const rc7 = `{
    "name": "SendMessage",
    "arguments": {
      "to": "main",
      "summary": "greeting",
      "message": "Hello! How can I assist you today?"
    }
  }`

	for _, text := range []string{
		rc7,
		"<function=SendMessage>\n<parameter=to>\nmain\n</parameter>\n</function>",
		`[TOOL_CALLS]SendMessage{"to":"main"}`,
	} {
		if c, ok := recoverToolCall(text, newOfferedTools(readTools())); ok {
			t.Errorf("recovered %q from an unoffered name; text=%q", c.Name, text)
		}
	}
}

// Product contract: with no tools in the request there is nothing to
// validate a name against, so recovery cannot run at all.
func TestRecoverToolCall_noToolsOfferedNeverFires(t *testing.T) {
	if _, ok := recoverToolCall(fencedJSONTranscript, nil); ok {
		t.Error("recovered a call from a request that offered no tools")
	}
}

// Product contract: the pair (string "name" + an argument object) is
// required. A model discussing JSON, or returning a record that happens
// to have a "name", must not read as a tool call.
func TestRecoverToolCall_ordinaryJSONIsNotACall(t *testing.T) {
	for _, text := range []string{
		`The config is {"name": "Read", "version": 2} — note the shape.`,
		`{"arguments": {"file_path": "/etc/hostname"}}`,
		"Here is a map: {\"Read\": {\"file_path\": \"/x\"}}",
	} {
		if c, ok := recoverToolCall(text, newOfferedTools(readTools())); ok {
			t.Errorf("recovered %q from ordinary JSON: %q", c.Name, text)
		}
	}
}

// Records today's behaviour of the brace scanner: a "{" inside a JSON
// string literal must not desynchronise the span search, or the object
// after it is never found.
func TestRecoverToolCall_bracesInsideStringsDoNotDesync(t *testing.T) {
	text := `{"note":"a brace { here"} then ` +
		`{"name":"Read","arguments":{"file_path":"/etc/hostname"}}`
	c, ok := recoverToolCall(text, newOfferedTools(readTools()))
	if !ok {
		t.Fatal("no call recovered")
	}
	if c.Name != "Read" {
		t.Errorf("tool = %q, want Read", c.Name)
	}
}

// Product contract: dialects that serialise every value as text get the
// declared types back, because a client validates the recovered call
// against its own schema and rejects `"5"` where it declared a number. A
// recovery the client throws away is not a recovery.
func TestRecoverToolCall_argumentsTypedFromSchema(t *testing.T) {
	text := "<function=Read>\n" +
		"<parameter=file_path>\n/etc/hostname\n</parameter>\n" +
		"<parameter=limit>\n5\n</parameter>\n" +
		"<parameter=raw>\ntrue\n</parameter>\n" +
		"</function>"
	c, ok := recoverToolCall(text, newOfferedTools(readTools()))
	if !ok {
		t.Fatal("no call recovered")
	}
	var got map[string]any
	if err := json.Unmarshal(c.Input, &got); err != nil {
		t.Fatalf("input not an object: %v", err)
	}
	if got["file_path"] != "/etc/hostname" {
		t.Errorf("file_path = %#v, want a string", got["file_path"])
	}
	if got["limit"] != float64(5) {
		t.Errorf("limit = %#v, want the number 5", got["limit"])
	}
	if got["raw"] != true {
		t.Errorf("raw = %#v, want the boolean true", got["raw"])
	}
}

// Product contract: coercion follows the schema, never the value's
// appearance. A string parameter that looks numeric stays a string —
// a zero-padded id or a version must survive intact.
func TestRecoverToolCall_numericLookingStringStaysAString(t *testing.T) {
	text := "<function=Read>\n<parameter=file_path>\n007\n</parameter>\n</function>"
	c, ok := recoverToolCall(text, newOfferedTools(readTools()))
	if !ok {
		t.Fatal("no call recovered")
	}
	if string(c.Input) != `{"file_path":"007"}` {
		t.Errorf("input = %s, want file_path to stay the string \"007\"", c.Input)
	}
}

// Product contract: block order stays thinking → text → tool_use when a
// call is recovered, and the recovered block is a normal tool_use the
// client cannot distinguish from one the engine parsed.
func TestOpenAIToAnthropic_RecoveredBlockOrdering(t *testing.T) {
	in := OpenAIResponse{
		ID: "chatcmpl-1",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:      "assistant",
				Reasoning: "The user wants the hostname.",
				Content:   xmlFunctionTranscript,
			},
			FinishReason: "stop",
		}},
	}
	out := OpenAIToAnthropic(in, "waired/default", readTools())

	if out.ToolRecovery != toolRecoveryXML {
		t.Errorf("ToolRecovery = %q, want %q", out.ToolRecovery, toolRecoveryXML)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
	var kinds []string
	for _, b := range out.Content {
		kinds = append(kinds, b.Type)
	}
	if len(kinds) != 3 || kinds[0] != "thinking" || kinds[1] != "text" || kinds[2] != "tool_use" {
		t.Fatalf("block order = %v, want [thinking text tool_use]", kinds)
	}
	if out.Content[2].ID == "" {
		t.Error("the recovered tool_use has no id; a client cannot pair a tool_result to it")
	}
}

// Product contract: ToolRecovery is bookkeeping, never part of the
// Anthropic response — a client must not be able to tell a recovered
// call from an engine-parsed one by inspecting the body.
func TestOpenAIToAnthropic_ToolRecoveryIsNotOnTheWire(t *testing.T) {
	in := OpenAIResponse{
		ID:      "chatcmpl-1",
		Choices: []OpenAIChoice{{Message: OpenAIMessage{Role: "assistant", Content: fencedJSONTranscript}, FinishReason: "stop"}},
	}
	encoded, err := json.Marshal(OpenAIToAnthropic(in, "waired/default", readTools()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "tool_recovery") || strings.Contains(string(encoded), "ToolRecovery") {
		t.Errorf("the recovery marker leaked onto the wire: %s", encoded)
	}
}

// Product contract: a response the engine parsed itself is passed
// through untouched, recovery never second-guesses it.
func TestOpenAIToAnthropic_StructuredCallSuppressesRecovery(t *testing.T) {
	in := OpenAIResponse{
		ID: "chatcmpl-1",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:    "assistant",
				Content: fencedJSONTranscript, // call-shaped prose alongside a real call
				ToolCalls: []OpenAIToolCall{{
					ID: "call_1", Type: "function",
					Function: OpenAIToolCallFunction{Name: "Bash", Arguments: `{"command":"ls"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out := OpenAIToAnthropic(in, "waired/default", readTools())

	if out.ToolRecovery != "" {
		t.Errorf("ToolRecovery = %q, want empty when the engine's parser worked", out.ToolRecovery)
	}
	toolUses := 0
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			toolUses++
		}
	}
	if toolUses != 1 {
		t.Errorf("%d tool_use blocks, want only the engine's own", toolUses)
	}
	if out.Content[0].Type != "text" || out.Content[0].Text != fencedJSONTranscript {
		t.Errorf("the text block was altered: %+v", out.Content[0])
	}
}

// Product contract for the streaming reassembler's keying. `index` is
// authoritative; without it an `id` identifies the call and a delta
// carrying neither continues the one in flight. Before #409 this was
// `key := len(tools)`, which made every argument continuation start a
// new call and split the argument JSON across blocks.
func TestToolDeltaKey(t *testing.T) {
	idx := func(i int) *int { return &i }

	t.Run("index is authoritative", func(t *testing.T) {
		tools := map[int]*partialTool{}
		var order []int
		next := 0
		if got := toolDeltaKey(idx(2), "", tools, order, &next); got != 2 {
			t.Errorf("key = %d, want 2", got)
		}
	})

	t.Run("argument continuation joins the call in flight", func(t *testing.T) {
		tools := map[int]*partialTool{0: {ID: "call_1", Name: "Read"}}
		order := []int{0}
		next := 1
		if got := toolDeltaKey(nil, "", tools, order, &next); got != 0 {
			t.Errorf("key = %d, want 0 (the call in flight)", got)
		}
	})

	t.Run("a known id reuses its partial", func(t *testing.T) {
		tools := map[int]*partialTool{0: {ID: "call_1"}, 1: {ID: "call_2"}}
		order := []int{0, 1}
		next := 2
		if got := toolDeltaKey(nil, "call_1", tools, order, &next); got != 0 {
			t.Errorf("key = %d, want 0", got)
		}
	})

	t.Run("a new id starts a new partial", func(t *testing.T) {
		tools := map[int]*partialTool{0: {ID: "call_1"}}
		order := []int{0}
		next := 1
		if got := toolDeltaKey(nil, "call_9", tools, order, &next); got != 1 {
			t.Errorf("key = %d, want a fresh key", got)
		}
		if next != 2 {
			t.Errorf("next = %d, want it advanced past the new key", next)
		}
	})
}

// Records today's behaviour: a call cut off mid-stream still recovers.
// A truncated call the client can reject beats handing the user raw
// template syntax and stalling the loop.
func TestRecoverToolCall_unterminatedXMLStillRecovers(t *testing.T) {
	text := "<function=Bash>\n<parameter=command>\nls -la\n</parameter>"
	c, ok := recoverToolCall(text, newOfferedTools(readTools()))
	if !ok {
		t.Fatal("no call recovered from a truncated fragment")
	}
	if c.Name != "Bash" || !strings.Contains(string(c.Input), "ls -la") {
		t.Errorf("recovered %q with input %s", c.Name, c.Input)
	}
}
