package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AnthropicRequest mirrors POST /v1/messages. Only the Phase A subset
// is decoded into named fields; everything else round-trips through
// the raw map so we don't drop fields a future version of the spec
// might add.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []AnthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

// AnthropicMessage's Content can be string OR []AnthropicContentBlock;
// keep raw and let the conversion code pick.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicResponse is the non-streaming response shape.
type AnthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []AnthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason,omitempty"`
	Usage      AnthropicUsage          `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// OpenAIRequest is the subset of /v1/chat/completions we synthesise
// from an AnthropicRequest. Only fields Ollama honours are populated.
type OpenAIRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Messages      []OpenAIMessage      `json:"messages"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *OpenAIStreamOptions `json:"stream_options,omitempty"`
	Tools         []OpenAITool         `json:"tools,omitempty"`
	ToolChoice    json.RawMessage      `json:"tool_choice,omitempty"`
	Stop          []string             `json:"stop,omitempty"`
}

// OpenAIStreamOptions opts a streaming request in to a trailing usage
// chunk. OpenAI-compatible engines (Ollama included) only emit token
// usage on the stream when include_usage is set; without it the final
// message_delta reports output_tokens: 0.
type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Reasoning carries a thinking model's chain-of-thought on the
	// response decode path. Ollama's OpenAI-compat surface uses
	// `reasoning`; vLLM / DeepSeek / some llama.cpp builds use
	// `reasoning_content`. Both are omitempty so they never appear on
	// the request we build. Read them via reasoningText().
	Reasoning        string           `json:"reasoning,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
}

// reasoningText returns the model's reasoning trace, preferring the
// `reasoning` field and falling back to `reasoning_content` for engines
// that use the alternate key.
func reasoningText(m OpenAIMessage) string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.ReasoningContent
}

type OpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function OpenAIToolCallFunction `json:"function"`
}

type OpenAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

type OpenAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// OpenAIResponse is the subset we decode from /v1/chat/completions.
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ErrUnsupportedFeature is returned by AnthropicToOpenAI when the
// request asks for an Anthropic feature that Phase A intentionally
// declines (vision, extended thinking, cache_control, system as
// array, …). The handler maps it to a 400 with a documented code.
type ErrUnsupportedFeature struct{ Feature, Detail string }

func (e *ErrUnsupportedFeature) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("anthropic feature %q not supported in Phase A: %s", e.Feature, e.Detail)
	}
	return fmt.Sprintf("anthropic feature %q not supported in Phase A", e.Feature)
}

// AnthropicToOpenAI translates the request body. The original
// model field is preserved (the gateway will swap it for the
// engine-specific identifier later, after the router has run).
func AnthropicToOpenAI(req AnthropicRequest) (OpenAIRequest, error) {
	if req.MaxTokens <= 0 {
		// Anthropic spec requires max_tokens.
		return OpenAIRequest{}, errors.New("anthropic: max_tokens is required")
	}

	out := OpenAIRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Stop:        req.StopSequences,
		ToolChoice:  req.ToolChoice,
	}
	// Opt in to the trailing usage chunk so streamed responses can
	// report real output_tokens (see OpenAIStreamOptions).
	if req.Stream {
		out.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	// system may arrive as a plain string OR as an array of text blocks
	// (Claude Code always sends the array form, attaching cache_control to
	// the blocks for prompt caching). Flatten both into a single system
	// message; cache_control is dropped (the local engine doesn't cache).
	sysStr, err := anthropicSystemToString(req.System)
	if err != nil {
		return OpenAIRequest{}, err
	}
	if sysStr != "" {
		out.Messages = append(out.Messages, OpenAIMessage{Role: "system", Content: sysStr})
	}

	// messages
	for _, m := range req.Messages {
		converted, err := convertAnthropicMessage(m)
		if err != nil {
			return OpenAIRequest{}, err
		}
		out.Messages = append(out.Messages, converted...)
	}

	// tools
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	// top_k is not supported by OpenAI Chat Completions; we silently
	// drop it. Caller can choose to surface a warning via header.
	_ = req.TopK

	return out, nil
}

// anthropicSystemToString collapses the Anthropic `system` field into a
// single string. It accepts the two shapes Anthropic (and Claude Code)
// send: a plain JSON string, or an array of content blocks. Text blocks
// are concatenated (newline-joined); cache_control and other block
// metadata have no field in AnthropicContentBlock, so they're dropped on
// unmarshal. Non-text system blocks (none are emitted in practice) are
// skipped. A value that is neither a string nor a block array is a
// malformed request and surfaces as a 400.
func anthropicSystemToString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("anthropic: system must be a string or array of blocks: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// convertAnthropicMessage translates a single Anthropic message,
// possibly fanning it out into multiple OpenAI messages (tool_result
// blocks become separate {role:"tool"} messages, per OpenAI's tool
// calling contract).
func convertAnthropicMessage(m AnthropicMessage) ([]OpenAIMessage, error) {
	// Try string-content first.
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		return []OpenAIMessage{{Role: m.Role, Content: asString}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("anthropic: content must be string or array of blocks: %w", err)
	}

	var out []OpenAIMessage
	var textParts []string
	var toolCalls []OpenAIToolCall

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: OpenAIToolCallFunction{Name: b.Name, Arguments: args},
			})
		case "tool_result":
			// tool_result becomes a separate role:"tool" message.
			content, err := stringifyToolResultContent(b.Content)
			if err != nil {
				return nil, err
			}
			// Flush accumulated text/tool_calls first so message
			// order matches Anthropic's order.
			if len(textParts) > 0 || len(toolCalls) > 0 {
				out = append(out, OpenAIMessage{
					Role:      m.Role,
					Content:   strings.Join(textParts, ""),
					ToolCalls: toolCalls,
				})
				textParts = nil
				toolCalls = nil
			}
			out = append(out, OpenAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    content,
			})
		case "image":
			return nil, &ErrUnsupportedFeature{Feature: "image", Detail: "vision content blocks land in Phase B"}
		case "thinking", "redacted_thinking":
			// Assistant reasoning blocks from a prior extended-thinking
			// turn (e.g. served by real Anthropic before the request was
			// routed to local inference). They have no OpenAI Chat
			// representation, so drop them rather than 400 the whole turn
			// — a model switch mid-conversation must not hard-fail.
			continue
		default:
			return nil, &ErrUnsupportedFeature{Feature: b.Type, Detail: "unknown content block"}
		}
	}

	if len(textParts) > 0 || len(toolCalls) > 0 {
		out = append(out, OpenAIMessage{
			Role:      m.Role,
			Content:   strings.Join(textParts, ""),
			ToolCalls: toolCalls,
		})
	}
	return out, nil
}

// stringifyToolResultContent collapses Anthropic tool_result content
// (string OR array of {type:"text",text}) into the plain string
// OpenAI's role:"tool" message expects.
func stringifyToolResultContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("anthropic: tool_result content must be string or text-block array: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", &ErrUnsupportedFeature{Feature: "tool_result_block_" + b.Type, Detail: "only text blocks in tool_result for Phase A"}
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, ""), nil
}

// OpenAIToAnthropic translates the non-streaming response back. The
// originalModel param is the user's requested model alias; we put
// that in the response.model field so the caller doesn't see the
// engine-specific identifier (Anthropic spec doesn't define what
// `model` should look like, but client SDKs cache by it).
func OpenAIToAnthropic(resp OpenAIResponse, originalModel string) AnthropicResponse {
	out := AnthropicResponse{
		ID:    "msg_" + resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: originalModel,
		// Initialise content as an empty (but non-nil) slice so it
		// marshals as `[]` not `null` — Anthropic SDK clients
		// expect an array even when the model produced nothing
		// visible (e.g. when reasoning consumed the whole budget).
		Content: []AnthropicContentBlock{},
		Usage: AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	if len(resp.Choices) == 0 {
		return out
	}
	choice := resp.Choices[0]
	// Block order follows Anthropic convention: thinking → text →
	// tool_use. Thinking models (e.g. qwen3) return their reasoning in
	// message.reasoning; surface it as a thinking block so Claude Code
	// can display it instead of the model appearing to emit no thinking.
	if r := reasoningText(choice.Message); r != "" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type:     "thinking",
			Thinking: r,
		})
	}
	if choice.Message.Content != "" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	// Safety net: if the model produced no visible block at all and the
	// stop reason is a max_tokens truncation (e.g. reasoning ate the
	// whole budget on a build that emits no reasoning field), a bare
	// content:[] turn stalls the agentic loop ("No response requested.").
	// Emit one visible note so the client always gets an actionable turn.
	if len(out.Content) == 0 && choice.FinishReason == "length" {
		out.Content = append(out.Content, AnthropicContentBlock{
			Type: "text",
			Text: truncationNote,
		})
	}
	out.StopReason = mapFinishReason(choice.FinishReason)
	return out
}

// truncationNote is the visible text emitted when a response truncates
// at max_tokens without producing any content, thinking, or tool call.
const truncationNote = "[waired: the model reached max_tokens before producing any output. Increase max_tokens to get a response.]"

func mapFinishReason(openai string) string {
	switch openai {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	}
	return openai
}

// CountTokensApprox returns a quick token-count estimate for an
// Anthropic /count_tokens request. Phase A uses a rough
// 1-token-per-4-bytes heuristic plus a per-message overhead. The
// response includes a Warning header (set by the handler) so clients
// know it's not exact; Phase B will use the model's real tokenizer.
func CountTokensApprox(req AnthropicRequest) int {
	const overheadPerMessage = 4
	total := 0
	if s, err := anthropicSystemToString(req.System); err == nil {
		total += approxTokenCount(s)
	}
	for _, m := range req.Messages {
		total += overheadPerMessage
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			total += approxTokenCount(s)
			continue
		}
		var blocks []AnthropicContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			for _, b := range blocks {
				total += approxTokenCount(b.Text)
				if len(b.Input) > 0 {
					total += approxTokenCount(string(b.Input))
				}
			}
		}
	}
	return total
}

func approxTokenCount(s string) int {
	// 1 token ≈ 4 bytes for English; coarse but good enough for the
	// "give me a rough budget" use case count_tokens serves.
	if s == "" {
		return 0
	}
	n := (len(s) + 3) / 4
	if n < 1 {
		n = 1
	}
	return n
}
