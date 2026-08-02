package agentgrade

import (
	"strings"
	"testing"
)

// The frames below are written in the exact shape internal/gateway emits
// (see proxyAnthropicStream). TestStreamMatchesGateway in
// stream_gateway_test.go is what keeps that claim honest against the real
// encoder; these tests cover the decoding rules a hand-written frame can
// state more precisely than a live turn can.

func sse(frames ...string) string { return strings.Join(frames, "") }

func frame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

const (
	frMsgStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" +
		"{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\"," +
		"\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n"
	frMsgDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":" +
		"{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":42}}\n\n"
	frMsgStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func textStart(idx int) string {
	return frame("content_block_start", `{"type":"content_block_start","index":`+itoa(idx)+
		`,"content_block":{"type":"text","text":""}}`)
}

func textDelta(idx int, s string) string {
	return frame("content_block_delta", `{"type":"content_block_delta","index":`+itoa(idx)+
		`,"delta":{"type":"text_delta","text":`+quote(s)+`}}`)
}

func blockStop(idx int) string {
	return frame("content_block_stop", `{"type":"content_block_stop","index":`+itoa(idx)+`}`)
}

func toolStart(idx int, id, name string) string {
	return frame("content_block_start", `{"type":"content_block_start","index":`+itoa(idx)+
		`,"content_block":{"type":"tool_use","id":`+quote(id)+`,"name":`+quote(name)+`,"input":{}}}`)
}

func toolDelta(idx int, partial string) string {
	return frame("content_block_delta", `{"type":"content_block_delta","index":`+itoa(idx)+
		`,"delta":{"type":"input_json_delta","partial_json":`+quote(partial)+`}}`)
}

// Product contract: a turn carrying text and a tool call arrives at the
// classifier as the same two blocks the whole-body path would have
// produced.
func TestReadAnthropicStream_TextAndToolUse(t *testing.T) {
	in := sse(frMsgStart,
		textStart(0), textDelta(0, "Let me "), textDelta(0, "look."), blockStop(0),
		toolStart(1, "toolu_1", "Read"),
		toolDelta(1, `{"file_path":"/etc/hostname"}`), blockStop(1),
		frMsgDelta, frMsgStop)

	resp, err := readAnthropicStream(strings.NewReader(in))
	if err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Let me look." {
		t.Errorf("text block = %+v", resp.Content[0])
	}
	if got := resp.Content[1]; got.Type != "tool_use" || got.Name != "Read" ||
		got.ID != "toolu_1" || string(got.Input) != `{"file_path":"/etc/hostname"}` {
		t.Errorf("tool block = %+v (input %s)", got, got.Input)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage.OutputTokens != 42 {
		t.Errorf("output_tokens = %d, want 42", resp.Usage.OutputTokens)
	}
}

// Engines stream argument JSON a token at a time, so the arguments of a
// single call routinely arrive split. Concatenating them in order is the
// whole job; getting it wrong yields invalid JSON and a verdict of
// "malformed tool call" against a model that emitted a fine one.
func TestReadAnthropicStream_SplitArgumentJSON(t *testing.T) {
	whole := `{"pattern":"quality_tier","path":"internal/router"}`
	var frames []string
	frames = append(frames, frMsgStart, toolStart(0, "toolu_2", "Grep"))
	for i := 0; i < len(whole); i += 5 {
		frames = append(frames, toolDelta(0, whole[i:min(i+5, len(whole))]))
	}
	frames = append(frames, blockStop(0), frMsgDelta, frMsgStop)

	resp, err := readAnthropicStream(strings.NewReader(sse(frames...)))
	if err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1", len(resp.Content))
	}
	if string(resp.Content[0].Input) != whole {
		t.Errorf("input = %s, want %s", resp.Content[0].Input, whole)
	}
}

// The gateway streams a thinking block at index 0 and shifts text to 1.
func TestReadAnthropicStream_ThinkingThenText(t *testing.T) {
	in := sse(frMsgStart,
		frame("content_block_start", `{"type":"content_block_start","index":0,`+
			`"content_block":{"type":"thinking","thinking":""}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,`+
			`"delta":{"type":"thinking_delta","thinking":"weighing it"}}`),
		blockStop(0),
		textStart(1), textDelta(1, "Hello."), blockStop(1),
		frMsgDelta, frMsgStop)

	resp, err := readAnthropicStream(strings.NewReader(in))
	if err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "weighing it" {
		t.Errorf("thinking block = %+v", resp.Content[0])
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "Hello." {
		t.Errorf("text block = %+v", resp.Content[1])
	}
}

// Index is a label on the wire, not an identity: the gateway reuses one
// for the recovered call, and two blocks that shared an index must not
// collapse into one.
func TestReadAnthropicStream_ReusedIndexKeepsBothBlocks(t *testing.T) {
	in := sse(frMsgStart,
		textStart(0), textDelta(0, "first"), blockStop(0),
		toolStart(0, "toolu_3", "Bash"), toolDelta(0, `{"command":"ls"}`), blockStop(0),
		frMsgDelta, frMsgStop)

	resp, err := readAnthropicStream(strings.NewReader(in))
	if err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Type != "text" || resp.Content[1].Type != "tool_use" {
		t.Errorf("blocks = %+v", resp.Content)
	}
}

// content_block_start declares an empty input object; a call whose
// arguments never arrive still has to decode as one.
func TestReadAnthropicStream_ToolWithNoArgumentDelta(t *testing.T) {
	in := sse(frMsgStart, toolStart(0, "toolu_4", "Glob"), blockStop(0), frMsgDelta, frMsgStop)
	resp, err := readAnthropicStream(strings.NewReader(in))
	if err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
	if len(resp.Content) != 1 || string(resp.Content[0].Input) != "{}" {
		t.Fatalf("blocks = %+v", resp.Content)
	}
}

func TestReadAnthropicStream_ToleratesDONE(t *testing.T) {
	in := sse(frMsgStart, textStart(0), textDelta(0, "hi"), blockStop(0), frMsgDelta, frMsgStop) +
		"data: [DONE]\n\n"
	if _, err := readAnthropicStream(strings.NewReader(in)); err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
}

// A stream cut short folds into a perfectly well-formed short answer, and
// "the model did not attempt a tool call" is the verdict that would be
// filed against a model that was mid-call. It has to be an error.
func TestReadAnthropicStream_TruncatedTurnIsAnError(t *testing.T) {
	in := sse(frMsgStart, textStart(0), textDelta(0, "I'll read it"), blockStop(0))
	_, err := readAnthropicStream(strings.NewReader(in))
	if err == nil {
		t.Fatal("truncated stream decoded without error")
	}
	if !strings.Contains(err.Error(), "incomplete SSE turn") {
		t.Errorf("error = %v", err)
	}
}

// Silently dropping a delta whose block is missing would lose exactly the
// fragment the probe exists to see.
func TestReadAnthropicStream_DeltaWithNoOpenBlockIsAnError(t *testing.T) {
	in := sse(frMsgStart, textDelta(3, "orphan"), frMsgStop)
	_, err := readAnthropicStream(strings.NewReader(in))
	if err == nil {
		t.Fatal("orphan delta decoded without error")
	}
	if !strings.Contains(err.Error(), "no open content block") {
		t.Errorf("error = %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
