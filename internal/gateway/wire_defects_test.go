package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Cover for the wire-handling defects found while building the
// request-shape matrix (waired-agent#1095). Each one made this
// gateway's own bug look like the model's, and none had a fixture.

func TestAnthropicToolChoiceTranslatesToTheOpenAIDialect(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"absent", "", ""},
		{"auto", `{"type":"auto"}`, `"auto"`},
		{"none", `{"type":"none"}`, `"none"`},
		{"any becomes required", `{"type":"any"}`, `"required"`},
		{"a named tool becomes a function", `{"type":"tool","name":"Read"}`,
			`{"function":{"name":"Read"},"type":"function"}`},
		{"a named tool with no name is dropped", `{"type":"tool"}`, ""},
		{"an OpenAI string passes through", `"required"`, `"required"`},
		{"an OpenAI object passes through", `{"type":"function","function":{"name":"Read"}}`,
			`{"type":"function","function":{"name":"Read"}}`},
		{"an unknown discriminator is dropped", `{"type":"telepathy"}`, ""},
		{"an unknown string is dropped", `"telepathy"`, ""},
		{"garbage is dropped", `[1,2,3]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in json.RawMessage
			if tc.in != "" {
				in = json.RawMessage(tc.in)
			}
			got := string(anthropicToolChoiceToOpenAI(in))
			if got != tc.want {
				t.Errorf("anthropicToolChoiceToOpenAI(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnthropicToolChoiceReachesTheEngine(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), `{
		"model":"waired/default","max_tokens":16,
		"tool_choice":{"type":"any"},
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"read a file"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var sent struct {
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal([]byte(captured), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(sent.ToolChoice); got != `"required"` {
		t.Errorf("engine saw tool_choice = %s, want \"required\"", got)
	}
}

func TestToolUseIDIsSynthesisedWhenTheEngineOmitsIt(t *testing.T) {
	resp := OpenAIResponse{
		ID: "cmpl-7",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{
					{ID: "", Function: OpenAIToolCallFunction{Name: "Read", Arguments: `{"path":"/etc/hostname"}`}},
					{ID: "", Function: OpenAIToolCallFunction{Name: "Grep", Arguments: `{"pattern":"x"}`}},
					{ID: "call_kept", Function: OpenAIToolCallFunction{Name: "Glob", Arguments: `{}`}},
				},
			},
			FinishReason: "tool_calls",
		}},
	}
	out := OpenAIToAnthropic(resp, "waired/default", nil)

	var ids []string
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("got %d tool_use blocks, want 3: %+v", len(ids), out.Content)
	}
	for i, id := range ids {
		if id == "" {
			t.Errorf("tool_use[%d] has no id: a client cannot pair a tool_result to it", i)
		}
	}
	if ids[2] != "call_kept" {
		t.Errorf("an id the engine supplied was replaced: %q", ids[2])
	}
	if ids[0] == ids[1] {
		t.Errorf("two synthesised ids collided: %q", ids[0])
	}
}

func TestToolCallInputStaysWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"an object passes through", `{"path":"/etc/hostname"}`, `{"path":"/etc/hostname"}`},
		{"empty becomes an empty object", "", `{}`},
		{"whitespace becomes an empty object", "   ", `{}`},
		{"a python-ish dict is preserved as a string", `{'path': '/etc/hostname'}`,
			`"{'path': '/etc/hostname'}"`},
		{"a bare word is preserved as a string", `hostname`, `"hostname"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolCallInput(tc.in)
			if !json.Valid(got) {
				t.Fatalf("toolCallInput(%q) = %s, which is not valid JSON", tc.in, got)
			}
			if string(got) != tc.want {
				t.Errorf("toolCallInput(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestMalformedToolArgumentsStillProduceAWholeBody is the end-to-end
// shape of the defect: the response used to fail to encode part-way
// through, and the client received HTTP 200 with a body that stopped
// mid-object.
func TestMalformedToolArgumentsStillProduceAWholeBody(t *testing.T) {
	upstream := fakeEngineStatus(t, http.StatusOK, `{
		"id":"cmpl-1","object":"chat.completion",
		"choices":[{"index":0,"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"call_1","type":"function",
				"function":{"name":"Read","arguments":"{'path': '/etc/hostname'}"}}]},
			"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`, nil)
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), `{
		"model":"waired/default","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("the client received a body it cannot parse: %v\nbody: %s", err, w.Body.String())
	}
	var sawTool bool
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			sawTool = true
			if !json.Valid(b.Input) {
				t.Errorf("tool_use input is not valid JSON: %s", b.Input)
			}
		}
	}
	if !sawTool {
		t.Error("the tool call vanished")
	}
}

// TestWriteJSONDoesNotHalfWriteABody pins the general guard: encoding
// happens before anything is written, so no value can leave the client
// with a status and half an object.
func TestWriteJSONDoesNotHalfWriteABody(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{
		"fine": "value",
		"bad":  math.NaN(), // json.Marshal refuses NaN
	})
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the error body is itself unparseable: %v\nbody: %s", err, w.Body.String())
	}
	if body["type"] != "error" {
		t.Errorf("body = %s, want an error shape", w.Body.String())
	}
}

func TestCutSSEData(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOK  bool
		comment string
	}{
		{"data: {\"a\":1}", `{"a":1}`, true, "the form every engine measured so far emits"},
		{"data:{\"a\":1}", `{"a":1}`, true, "the space is optional in the format"},
		{"data:  {\"a\":1}", ` {"a":1}`, true, "only ONE space is framing; the rest is data"},
		{"data: [DONE]", "[DONE]", true, ""},
		{"event: message", "", false, "not a data field"},
		{"", "", false, "the blank line between frames"},
	}
	for _, tc := range cases {
		got, ok := CutSSEData(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("CutSSEData(%q) = (%q, %v), want (%q, %v) — %s",
				tc.in, got, ok, tc.want, tc.wantOK, tc.comment)
		}
	}
}

// TestAnthropicStreamAcceptsDataWithoutASpace is the defect this
// gateway would have blamed on a model: the Anthropic reader required
// "data: " while the usage sniffer accepted "data:", so an engine that
// omitted the space produced a turn with no content whose token
// accounting was correct — an answerless clean finish, which is exactly
// the signature of a model failing mid-stream (#442).
func TestAnthropicStreamAcceptsDataWithoutASpace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		frames := []string{
			`data:{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`data:{"id":"c1","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			`data:{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data:[DONE]`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "%s\n\n", f)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), `{
		"model":"waired/default","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("the turn arrived empty; the engine's text was dropped for want of a space:\n%s", w.Body.String())
	}
}

// TestLargeSSEFrameIsNotAMidStreamFailure: a tool call's arguments can
// arrive as one frame, and a Write of a large file is one such frame.
// Over the reader's line limit the scanner stops with an error, which
// reads as a mid-stream truncation and is reported to the client as the
// model failing after three attempts that fail identically.
func TestLargeSSEFrameIsNotAMidStreamFailure(t *testing.T) {
	big := strings.Repeat("x", 1200*1024) // over the old 1 MiB bound
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		enc, err := json.Marshal(map[string]any{"content": big})
		if err != nil {
			t.Errorf("marshal: %v", err)
			return
		}
		frames := []string{
			`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			fmt.Sprintf(`data: {"id":"c1","choices":[{"index":0,"delta":%s}]}`, enc),
			`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "%s\n\n", f)
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	w := postAnthropicBody(t, anthropicGatewayFor(t, upstream.URL), `{
		"model":"waired/default","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"write a file"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "waired: the model") {
		t.Errorf("a frame over the old bound was reported as the model failing:\n%s", firstLines(body, 6))
	}
	if !strings.Contains(body, "xxxx") {
		t.Errorf("the large frame never reached the client:\n%s", firstLines(body, 6))
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestReasoningLeakOnlyTurnIsNotUsable: an engine whose parser did not
// split the reasoning channel hands the trace over as visible assistant
// text. A turn that is only that is not an answer.
func TestReasoningLeakOnlyTurnIsNotUsable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"a bare closing think tag", "</think>", true},
		{"an empty think block", "<think></think>", true},
		{"a think block around whitespace", "<think>\n\n</think>", true},

		// The predicate is subtractive: it removes what is recognisably
		// markup and requires that NOTHING else is left. A leaked trace
		// leaves its own prose behind, so these are NOT markup-only —
		// and must not be, or a turn that reasons and then answers would
		// be thrown away. The leak is reported instead; see
		// TestReasoningLeakIsReportedWithoutChangingTheVerdict.
		{"a whole think block leaves the trace behind",
			"<think>the user wants a file</think>", false},
		{"reasoning followed by an answer keeps the answer",
			"<think>weighing it up</think>The file contains: waired-dev", false},
		{"an ordinary answer", "The file contains: waired-dev", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := textIsOnlyEngineMarkup(tc.in); got != tc.want {
				t.Errorf("textIsOnlyEngineMarkup(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestReasoningLeakIsReportedWithoutChangingTheVerdict: an engine whose
// parser did not split the reasoning channel hands the trace over as
// visible assistant text. Nothing in the Go code looked for that — the
// only detector lived in scripts/dev/agentgrade-contract.py, out of band
// and read by nobody in CI.
//
// It is reported, not acted on. A turn that leaked its reasoning AND
// answered still answered, and dropping it would cost the user a reply
// to fix a presentation defect.
func TestReasoningLeakIsReportedWithoutChangingTheVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"a bare closing tag", "some reasoning</think>", true},
		{"a whole block", "<think>weighing it up</think>the answer", true},
		{"a harmony channel", "<|channel|>analysis<|message|>weighing it up", true},
		{"a harmony assistant start", "<|start|>assistant<|message|>hi", true},
		{"an ordinary answer", "The file contains: waired-dev", false},
		{"the word think in prose", "I think the file is empty", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := textLeaksReasoning(tc.in); got != tc.want {
				t.Errorf("textLeaksReasoning(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
