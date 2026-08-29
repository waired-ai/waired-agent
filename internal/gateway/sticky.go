package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// stickyHeader is the canonical opt-in header. Coding-agent clients
// (Claude Code, Codex, etc.) that thread conversation identity set
// this; the gateway respects it verbatim (after sanitisation).
const stickyHeader = "X-Waired-Conversation-Id"

// stickyPrefixBytes bounds how much of the request body the
// last-resort hash consumes. 1 KiB is cheap and, for a client that
// offers nothing better, is all there is.
//
// It is a LAST resort and not the rule any more (waired-agent#1125).
// Measured on a real `claude` CLI: two sessions opened in one
// repository differ first at byte 6,628 — 6.5x past this window,
// because everything before it is a shared system-reminder block — so
// the window collapsed them into one conversation. In the other
// direction `model` is the first key on the wire, so a /model pick
// changed the id of a conversation that had not changed at all. No
// window size fixes the second half: any prefix carries `model`.
const stickyPrefixBytes = 1024

// stickyIDBytes is how much of the digest becomes the id. 8 bytes =
// 16 hex chars = a 64-bit prefix, unchanged since Phase 7.
const stickyIDBytes = 8

// StickyIdentity is the conversation-identifying material a caller has
// ALREADY decoded. It exists so ComputeStickyID never decodes a body a
// second time: both surfaces unmarshal the request before they ask for
// an id, and a coding-agent body runs to hundreds of kilobytes.
//
// Every field is optional. A caller that has none gets the historical
// body-prefix behaviour.
type StickyIdentity struct {
	// Metadata is the Anthropic request's `metadata` object, verbatim.
	// Only `user_id` is read out of it.
	Metadata json.RawMessage

	// User is the OpenAI-native `user` member, verbatim (a JSON string).
	// It answers the same question `metadata.user_id` does on the other
	// surface.
	User json.RawMessage

	// FirstMessage is messages[0], verbatim.
	FirstMessage json.RawMessage
}

// ComputeStickyID returns the sticky-routing identifier for one
// request. Resolution chain:
//
//  1. The X-Waired-Conversation-Id header, sanitised (32 chars max,
//     alphanumeric / dash / underscore). Nothing in this repository
//     sets it; it is there for a client that threads its own identity.
//  2. A hash of the caller's identity material: the client's own user
//     id and its first message, whichever of the two it sent.
//  3. A hash of the first stickyPrefixBytes bytes of the body.
//
// Empty inputs produce an empty string — the Selector reads that as
// "no affinity hint" and routes purely by rank.
//
// # Why rung 2 uses BOTH halves
//
// Neither half is sufficient alone, and each covers the other's gap
// (all four rows measured on a real `claude` CLI, waired-agent#1125):
//
//   - The user id alone over-merges. Both `metadata.user_id` (Anthropic)
//     and `user` (OpenAI) are specified as identifying the PERSON, not
//     the conversation. Claude Code happens to put a session id inside
//     its value, but a client that sends a stable per-user string would
//     have every one of its conversations collapse onto one peer.
//   - The first message alone over-merges for a client that opens every
//     conversation with the same preamble.
//   - Together they separate two sessions started in one repository
//     (their first messages diverge at byte 6,628) and hold one session
//     together across turns (its first message is byte-identical turn to
//     turn, and its user id is stable across a `--continue`).
//
// # What is deliberately NOT in the hash
//
// The model id. It is the first key on the wire, so every byte-prefix
// window carries it, and `claude-waired-auto` / `-peer` / `-local` /
// `-cloud` are delivered to the client AS MODEL IDS (#830, #1036).
// Hashing it means picking a different `/model` entry drops the KV
// affinity binding and buys a full re-prefill — 35.38 s against a
// 33.85 s cold value on the host that measured it.
//
// The material itself never leaves this function. Claude Code's
// `metadata.user_id` carries device and session identifiers, so what is
// stored and logged is the digest, never the input.
func ComputeStickyID(headers http.Header, body []byte, id StickyIdentity) string {
	if explicit := headers.Get(stickyHeader); explicit != "" {
		return sanitiseStickyHeader(explicit)
	}
	if material := id.material(); len(material) > 0 {
		return stickyHash('i', material)
	}
	if len(body) == 0 {
		return ""
	}
	prefix := body
	if len(prefix) > stickyPrefixBytes {
		prefix = prefix[:stickyPrefixBytes]
	}
	return stickyHash('b', prefix)
}

// material concatenates whatever identity the client offered. The rung
// separator keeps a caller that sent only a user id from colliding with
// one that sent only a first message whose bytes happen to match.
func (id StickyIdentity) material() []byte {
	user := id.userID()
	if user == "" && len(id.FirstMessage) == 0 {
		return nil
	}
	out := make([]byte, 0, len(user)+len(id.FirstMessage)+1)
	out = append(out, user...)
	out = append(out, 0)
	out = append(out, id.FirstMessage...)
	return out
}

// userID reads the client's own identifier from whichever surface's
// field the caller filled in. A member that is absent, null or not a
// string is no claim, not an empty one.
func (id StickyIdentity) userID() string {
	if len(id.Metadata) > 0 {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(id.Metadata, &meta); err == nil && meta.UserID != "" {
			return meta.UserID
		}
	}
	if len(id.User) > 0 {
		var user string
		if err := json.Unmarshal(id.User, &user); err == nil {
			return user
		}
	}
	return ""
}

// stickyHash is the one digest in this file. The rung tag is mixed in
// so a body prefix can never collide with identity material.
func stickyHash(rung byte, material []byte) string {
	h := sha256.New()
	h.Write([]byte{rung})
	h.Write(material)
	return hex.EncodeToString(h.Sum(nil)[:stickyIDBytes])
}

// sanitiseStickyHeader keeps only [a-zA-Z0-9_-] and truncates to 32
// chars. This avoids unbounded growth in the sticky map (a hostile
// client otherwise could spam unique IDs to OOM the map) and keeps
// the identifier safe to embed in log lines / traces.
func sanitiseStickyHeader(in string) string {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in) && len(out) < 32; i++ {
		c := in[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_' || c == '-':
			out = append(out, c)
		}
	}
	return string(out)
}

// stickyIdentityFromAnthropic reads the identity out of a request the
// Anthropic handler has already unmarshalled. Re-marshalling
// messages[0] is deterministic — AnthropicMessage keeps its Content as
// json.RawMessage, so the client's own bytes come back out.
func stickyIdentityFromAnthropic(req AnthropicRequest) StickyIdentity {
	id := StickyIdentity{Metadata: req.Metadata}
	if len(req.Messages) > 0 {
		if encoded, err := json.Marshal(req.Messages[0]); err == nil {
			id.FirstMessage = encoded
		}
	}
	return id
}

// stickyIdentityFromOpenAIBody reads the identity out of a body the
// OpenAI handler has already decoded into its top-level members. It
// accepts `metadata` as well as `user` because newer OpenAI-shaped
// clients send both.
func stickyIdentityFromOpenAIBody(raw map[string]json.RawMessage) StickyIdentity {
	id := StickyIdentity{Metadata: raw["metadata"], User: raw["user"]}
	if messages, ok := raw["messages"]; ok {
		id.FirstMessage = firstJSONArrayElement(messages)
	}
	return id
}

// firstJSONArrayElement returns the first element of a JSON array,
// verbatim, without materialising the rest. The messages array is the
// bulk of a coding-agent body and only its head is wanted here.
func firstJSONArrayElement(arr json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(arr))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil
	}
	if !dec.More() {
		return nil
	}
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return nil
	}
	return first
}
