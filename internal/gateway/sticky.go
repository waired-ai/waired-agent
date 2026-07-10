package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// stickyHeader is the canonical opt-in header. Coding-agent clients
// (Claude Code, Codex, etc.) that thread conversation identity set
// this; the gateway respects it verbatim (after sanitisation).
const stickyHeader = "X-Waired-Conversation-Id"

// stickyPrefixBytes bounds how much of the request body the
// fallback hash consumes. 1 KiB is well below the typical chat
// prompt and large enough that a unique system prompt + first user
// turn dominates the hash output. Bigger windows offer diminishing
// affinity returns and pay an O(N) hash cost on every request.
const stickyPrefixBytes = 1024

// ComputeStickyID returns the sticky-routing identifier for one
// request. Resolution chain:
//
//  1. If the X-Waired-Conversation-Id header is set, sanitise it and
//     return it (32 char max, alphanumeric / dash / underscore).
//  2. Otherwise hash the first stickyPrefixBytes bytes of the
//     request body with SHA-256 and return the first 16 hex chars.
//
// Empty bodies with no header produce an empty string — the Selector
// treats that as "no affinity hint" and routes purely by score.
//
// The function is pure: no side effects, no allocations beyond the
// returned string. Callers feed `body` after reading the whole body
// (the existing MaxBytesReader path in handleOpenAIChatCompletions
// and handleAnthropicMessages already buffers).
func ComputeStickyID(headers http.Header, body []byte) string {
	if explicit := headers.Get(stickyHeader); explicit != "" {
		return sanitiseStickyHeader(explicit)
	}
	if len(body) == 0 {
		return ""
	}
	prefix := body
	if len(prefix) > stickyPrefixBytes {
		prefix = prefix[:stickyPrefixBytes]
	}
	sum := sha256.Sum256(prefix)
	return hex.EncodeToString(sum[:8]) // 16 hex chars = 64-bit prefix
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
