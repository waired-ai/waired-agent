package gateway

import (
	"encoding/json"
	"log/slog"
)

// Instruction-turn normalisation for the NATIVE OpenAI surface
// (waired-agent#1055).
//
// AnthropicToOpenAI folds a non-leading system / developer turn into the
// leading system message before anything reaches an engine
// (normalizeInstructionTurns, convert.go), because a strict chat
// template refuses the shape outright — qwen3.8's renderer answers
// `[user, system]` with 500 "system message must be at the beginning",
// and vLLM's Qwen Jinja template raises the same error
// (waired-agent#1035). The native surface never had that fold: it
// forwards the client's own message array, so the same conversation
// reaching /v1/chat/completions instead of /anthropic/v1/messages still
// fails.
//
// The path that makes this concrete is not an external client — no
// shipping client in this repository is shown to send the shape here —
// but the mesh ingress leg. handleOpenAIChatCompletions is also the
// entry point a peer's requester posts to, so during a rolling upgrade a
// requester that predates the fold hands an unfolded array to a serving
// peer that would otherwise have protected itself.
//
// Structurally separate from normalizeInstructionTurns rather than
// shared with it, for one reason: that one works on []OpenAIMessage,
// whose Content is a plain string because it is built by our own
// converter. A native client may send an array of content parts, which
// round-tripping through that struct would flatten or drop —
// CountOpenAIPromptTokensApprox already handles the array case, so the
// two would disagree about the same request. This one works on raw JSON
// and preserves every field it does not fold, known to us or not.

// normalizeOpenAIBodyInstructionTurns folds every instruction turn that
// is not the leading system message into the leading system message, in
// order, and reports whether it changed anything.
//
// Returns the argument unchanged whenever there is nothing to fold, and
// also whenever the body is not shaped the way this fold understands.
// Both cases forward exactly what the client sent, which is what this
// surface did before — a request this cannot normalise is no worse off
// than it was, and a 500 from a strict engine is a better outcome than
// silently rewriting a prompt we did not understand.
func normalizeOpenAIBodyInstructionTurns(body []byte) ([]byte, bool) {
	if !openAIBodyNeedsInstructionFold(body) {
		return body, false
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		return body, false
	}

	parts := make([]string, 0, 2)
	kept := make([]json.RawMessage, 0, len(msgs))
	for i, raw := range msgs {
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return body, false
		}
		// The leading system message is the destination, not a turn to
		// fold; every other instruction turn moves into it. A leading
		// `developer` turn is NOT the destination — it is an
		// instruction turn like any other and becomes part of the
		// system message this builds, which is what
		// normalizeInstructionTurns does with the same input.
		if !(i == 0 && head.Role == "system") && !isInstructionRole(head.Role) {
			kept = append(kept, raw)
			continue
		}
		text, ok := openAIInstructionText(head.Content)
		if !ok {
			// Content this fold cannot merge without inventing
			// something — an image part, an unknown part type. Leave
			// the whole request alone rather than move some of it.
			slog.Debug("openai instruction turn not folded: content is not text",
				"index", i, "role", head.Role)
			return body, false
		}
		if text != "" {
			parts = append(parts, text)
		}
	}

	out := kept
	if folded := joinInstructionParts(parts); folded != "" {
		lead, err := json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "system", Content: folded})
		if err != nil {
			return body, false
		}
		out = append([]json.RawMessage{lead}, kept...)
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return body, false
	}
	top["messages"] = encoded
	rewritten, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// openAIBodyNeedsInstructionFold decodes the roles alone, so the common
// case — a conversation that was already legal — costs one narrow
// decode and no re-marshal at all.
func openAIBodyNeedsInstructionFold(body []byte) bool {
	var probe struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	for i, m := range probe.Messages {
		if i == 0 && m.Role == "system" {
			continue
		}
		if isInstructionRole(m.Role) {
			return true
		}
	}
	return false
}

// openAIInstructionText reads one message's content as the text an
// instruction turn contributes, and reports whether it could.
//
// Absent or null content is empty text, not a failure: a contentless
// system turn carries no instructions and folding it away loses
// nothing. An array of content parts is read only when every part is a
// text part; the parts of one message are pieces of one string, so they
// join with no separator between them.
func openAIInstructionText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false
	}
	out := ""
	for _, p := range parts {
		if p.Type != "text" {
			return "", false
		}
		out += p.Text
	}
	return out, true
}

// joinInstructionParts joins the folded turns the way
// normalizeInstructionTurns does, so a request normalised here and the
// same conversation normalised on the Anthropic surface render the same
// prompt.
func joinInstructionParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += instructionTurnSeparator
		}
		out += p
	}
	return out
}
