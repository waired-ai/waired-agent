package gateway

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAnthropicToOpenAI_StringContent(t *testing.T) {
	in := AnthropicRequest{
		Model:     "waired/default",
		MaxTokens: 64,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" || out.Messages[0].Content != "hi" {
		t.Errorf("messages = %+v", out.Messages)
	}
	if out.MaxTokens != 64 {
		t.Errorf("MaxTokens = %d", out.MaxTokens)
	}
}

func TestAnthropicToOpenAI_RequiresMaxTokens(t *testing.T) {
	in := AnthropicRequest{Model: "x", Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	if _, err := AnthropicToOpenAI(in); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("expected max_tokens error, got %v", err)
	}
}

func TestAnthropicToOpenAI_SystemAsString(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		System:   json.RawMessage(`"you are helpful"`),
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, _ := AnthropicToOpenAI(in)
	if len(out.Messages) < 2 || out.Messages[0].Role != "system" || out.Messages[0].Content != "you are helpful" {
		t.Errorf("system not prepended, messages = %+v", out.Messages)
	}
}

func TestAnthropicToOpenAI_SystemAsArrayFlattened(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		System: json.RawMessage(`[
			{"type":"text","text":"line one"},
			{"type":"text","text":"line two"}
		]`),
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Messages) < 2 || out.Messages[0].Role != "system" || out.Messages[0].Content != "line one\nline two" {
		t.Errorf("system blocks not flattened, messages = %+v", out.Messages)
	}
}

// Claude Code always sends `system` as an array of text blocks and marks
// prompt-cache breakpoints with cache_control. The gateway must flatten the
// text and ignore cache_control rather than 400 (was the "system_blocks"
// unsupported-feature error).
func TestAnthropicToOpenAI_SystemBlocksWithCacheControl(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		System: json.RawMessage(`[
			{"type":"text","text":"You are Claude Code."},
			{"type":"text","text":"<context>","cache_control":{"type":"ephemeral"}}
		]`),
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "You are Claude Code.\n<context>" {
		t.Errorf("system = %+v", out.Messages[0])
	}
}

// An assistant history entry carrying a thinking block (e.g. produced by a
// prior real-Anthropic turn before the request was routed to local
// inference) must not 400 the whole turn: the thinking block is dropped and
// the visible text kept.
func TestAnthropicToOpenAI_ThinkingBlockDropped(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"thinking","thinking":"let me think"},
				{"type":"text","text":"the answer"}
			]`)},
		},
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("thinking block should be dropped, got error: %v", err)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "assistant" || last.Content != "the answer" {
		t.Errorf("assistant message = %+v", last)
	}
}

func TestAnthropicToOpenAI_TextBlocksConcatenated(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[
			{"type":"text","text":"one "},
			{"type":"text","text":"two"}
		]`)}},
	}
	out, _ := AnthropicToOpenAI(in)
	if out.Messages[0].Content != "one two" {
		t.Errorf("content = %q", out.Messages[0].Content)
	}
}

func TestAnthropicToOpenAI_ToolUseBecomesToolCalls(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages: []AnthropicMessage{{Role: "assistant", Content: json.RawMessage(`[
			{"type":"text","text":"using a tool"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Tokyo"}}
		]`)}},
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages len = %d", len(out.Messages))
	}
	if out.Messages[0].Content != "using a tool" {
		t.Errorf("content = %q", out.Messages[0].Content)
	}
	if len(out.Messages[0].ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", out.Messages[0].ToolCalls)
	}
	tc := out.Messages[0].ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "get_weather" || !strings.Contains(tc.Function.Arguments, "Tokyo") {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestAnthropicToOpenAI_ToolResultBecomesToolMessage(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"22C sunny"}
			]`)},
		},
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "tool" {
		t.Fatalf("messages = %+v", out.Messages)
	}
	if out.Messages[0].ToolCallID != "toolu_1" || out.Messages[0].Content != "22C sunny" {
		t.Errorf("tool message = %+v", out.Messages[0])
	}
}

func TestAnthropicToOpenAI_ImageUnsupported(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[
			{"type":"image","source":{}}
		]`)}},
	}
	_, err := AnthropicToOpenAI(in)
	var unsup *ErrUnsupportedFeature
	if !errors.As(err, &unsup) || unsup.Feature != "image" {
		t.Errorf("expected image unsupported, got %v", err)
	}
}

func TestAnthropicToOpenAI_ToolsConverted(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []AnthropicTool{{
			Name: "get_weather", Description: "look up weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	out, _ := AnthropicToOpenAI(in)
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	if !strings.Contains(string(out.Tools[0].Function.Parameters), "city") {
		t.Errorf("parameters = %s", out.Tools[0].Function.Parameters)
	}
}

func TestAnthropicToOpenAI_StopSequences(t *testing.T) {
	in := AnthropicRequest{
		Model: "x", MaxTokens: 16,
		Messages:      []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		StopSequences: []string{"\n\n", "STOP"},
	}
	out, _ := AnthropicToOpenAI(in)
	if len(out.Stop) != 2 || out.Stop[1] != "STOP" {
		t.Errorf("Stop = %v", out.Stop)
	}
}

func TestOpenAIToAnthropic_TextOnly(t *testing.T) {
	in := OpenAIResponse{
		ID: "chatcmpl-1",
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Role: "assistant", Content: "hello!"},
			FinishReason: "stop",
		}},
		Usage: OpenAIUsage{PromptTokens: 12, CompletionTokens: 3},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("envelope = %+v", out)
	}
	if out.Model != "waired/default" {
		t.Errorf("model = %q, want waired/default", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "hello!" {
		t.Errorf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", out.StopReason)
	}
	if out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestOpenAIToAnthropic_ReasoningBecomesThinking(t *testing.T) {
	// A thinking model returns its chain-of-thought in message.reasoning
	// alongside the visible answer. It must surface as a thinking block
	// ordered before the text block.
	in := OpenAIResponse{
		ID: "chatcmpl-2",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:      "assistant",
				Reasoning: "1. 17*23 = 17*20 + 17*3 = 340 + 51",
				Content:   "391",
			},
			FinishReason: "stop",
		}},
		Usage: OpenAIUsage{PromptTokens: 20, CompletionTokens: 30},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if len(out.Content) != 2 {
		t.Fatalf("content = %+v, want [thinking, text]", out.Content)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "1. 17*23 = 17*20 + 17*3 = 340 + 51" {
		t.Errorf("block[0] = %+v, want thinking", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "391" {
		t.Errorf("block[1] = %+v, want text", out.Content[1])
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", out.StopReason)
	}
}

func TestOpenAIToAnthropic_ReasoningContentAlias(t *testing.T) {
	// Some engines (vLLM/DeepSeek/llama.cpp builds) use reasoning_content
	// instead of reasoning; both must map to a thinking block.
	in := OpenAIResponse{
		ID: "x",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:             "assistant",
				ReasoningContent: "let me think",
				Content:          "done",
			},
			FinishReason: "stop",
		}},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if len(out.Content) != 2 || out.Content[0].Type != "thinking" || out.Content[0].Thinking != "let me think" {
		t.Fatalf("content = %+v, want thinking(reasoning_content) then text", out.Content)
	}
}

func TestOpenAIToAnthropic_ReasoningOnlyMaxTokens(t *testing.T) {
	// Reasoning ate the whole budget: content empty, finish_reason
	// length. The thinking block keeps the turn non-empty (no stall),
	// and no synthetic note is added because a visible thinking block is
	// already present.
	in := OpenAIResponse{
		ID: "x",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:      "assistant",
				Reasoning: "Thinking Process: first I need to...",
				Content:   "",
			},
			FinishReason: "length",
		}},
		Usage: OpenAIUsage{PromptTokens: 25, CompletionTokens: 300},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if len(out.Content) != 1 || out.Content[0].Type != "thinking" {
		t.Fatalf("content = %+v, want a single thinking block", out.Content)
	}
	if out.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %q, want max_tokens", out.StopReason)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), `"content":[]`) {
		t.Errorf("reasoning-only turn must not be empty: %s", encoded)
	}
}

func TestOpenAIToAnthropic_ToolCallsBecomeBlocks(t *testing.T) {
	in := OpenAIResponse{
		ID: "x",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{{
					ID: "call_1", Type: "function",
					Function: OpenAIToolCallFunction{Name: "get_weather", Arguments: `{"city":"Tokyo"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Content[0].Name != "get_weather" || string(out.Content[0].Input) != `{"city":"Tokyo"}` {
		t.Errorf("tool_use block = %+v", out.Content[0])
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"":              "end_turn",
		"stop":          "end_turn",
		"length":        "max_tokens",
		"tool_calls":    "tool_use",
		"function_call": "tool_use",
		"weird":         "weird",
	}
	for in, want := range cases {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAIToAnthropic_EmptyContentIsNonNilArray(t *testing.T) {
	// An assistant turn with no visible block still marshals content as
	// []AnthropicContentBlock (never null) — Anthropic SDKs require an
	// array. finish_reason=stop here, so no truncation note is added.
	in := OpenAIResponse{
		ID: "x",
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Role: "assistant", Content: ""},
			FinishReason: "stop",
		}},
		Usage: OpenAIUsage{PromptTokens: 5, CompletionTokens: 0},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if out.Content == nil {
		t.Fatalf("Content should be non-nil empty slice, got nil")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"content":[]`) {
		t.Errorf("expected content:[] in output, got %s", encoded)
	}
}

func TestOpenAIToAnthropic_EmptyLengthGetsTruncationNote(t *testing.T) {
	// The degenerate stall shape: max_tokens truncation with no content
	// AND no reasoning field. A bare content:[] turn stalled the loop
	// overnight (#644), so a visible note is emitted instead.
	in := OpenAIResponse{
		ID: "x",
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Role: "assistant", Content: ""},
			FinishReason: "length",
		}},
		Usage: OpenAIUsage{PromptTokens: 5, CompletionTokens: 0},
	}
	out := OpenAIToAnthropic(in, "waired/default")
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != truncationNote {
		t.Fatalf("content = %+v, want a single truncation-note text block", out.Content)
	}
	if out.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %q, want max_tokens", out.StopReason)
	}
}

func TestCountTokensApprox_NonZero(t *testing.T) {
	in := AnthropicRequest{
		System:   json.RawMessage(`"You are helpful"`),
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello, please summarise the following document..."`)}},
	}
	got := CountTokensApprox(in)
	if got <= 0 {
		t.Errorf("approx tokens = %d, want > 0", got)
	}
}
