package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// The OpenAI-dialect half of waired-agent#837's keepalive (waired-agent#952).
//
// proxyToEngine is a byte pipe: it blocks in client.Do and the first thing
// the client sees is the engine's own status and headers, forwarded. Ollama
// withholds response headers until the weights are resident, so a streaming
// request against a model that is not loaded produces ZERO bytes for the
// whole load — the state the Anthropic leg was in before #837. Measured on
// the shipping consumer (#952's own comment): OpenCode held four connections
// open for 116 seconds with nothing to show for it, and the same silence had
// twice been diagnosed as a hang and killed.
//
// #952 listed four things that had to be settled before a byte could be
// written early. They are settled here:
//
//  1. WHETHER THE CLIENT ASKED FOR A STREAM. A keepalive on a non-streaming
//     request would be a protocol error, not a courtesy, so the handler now
//     reads `stream` out of the body map it already decodes and passes it
//     down. False means today's behaviour, byte for byte.
//  2. THE FRAME. The same SSE comment line the Anthropic leg writes. A
//     comment is the SSE specification's own keepalive construct, dropped by
//     every conformant decoder before event assembly, so it needs no promise
//     about OpenAI-dialect clients in particular.
//  3. THE OVERLAY STAYS OUT. Deps.StreamKeepalive is 0 on that listener and
//     stays 0: it is the SERVING side of a mesh leg, and a first byte there
//     is one the requesting peer's engine never produced, which disarms that
//     peer's #757 budget and makes every peer on the mesh look responsive.
//  4. WHAT A POST-COMMIT NON-2XX BECOMES. Answered by writeOpenAIErrorFrame
//     below.
//
// What is NOT carried over from the byte-pipe behaviour, once a frame has
// gone out: the engine's own headers cannot be forwarded (ours are already
// on the wire) and its status cannot be spent (HTTP gives no way to take a
// 200 back). Both are the same trade the Anthropic leg took, and both happen
// only on a request that asked for a stream and then waited longer than one
// keepalive interval for its first byte.

// writeOpenAIStreamHeaders commits an OpenAI-dialect SSE response.
//
// The engine's own headers are what this leg forwards when it can, so these
// are deliberately the smallest set that makes the response a stream, and
// the same three the Anthropic leg commits: the content type, no caching,
// and the hint that stops a reverse proxy buffering the body back into one
// piece.
func writeOpenAIStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeOpenAIErrorFrame renders a failure to a client that is already
// reading a committed OpenAI SSE stream (#952's fourth question).
//
// Pre-commit this leg forwards the engine's status and body verbatim, which
// is what it exists to do. Post-commit the status is spent, so the same
// envelope writeOpenAIError would have written goes out as one `data:`
// frame. Two reasons for that shape rather than an invented one: it is the
// body a client already has to be able to read on this leg, and it is the
// shape OpenAI's own streaming surface uses for a mid-stream failure. No
// `[DONE]` follows — that sentinel says the stream finished, and this one
// did not.
//
// The caller records the real status on rr regardless, which is what keeps
// the event ring honest about a turn the client received as a 200
// (waired-agent#538).
func writeOpenAIErrorFrame(w http.ResponseWriter, status int, errType, code, message string) {
	data, err := json.Marshal(openAIErrorEnvelope{
		Error: OpenAIError{Message: message, Type: errType, Code: code},
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_ = status // spent at commit; recorded by the caller, not written here.
}

// jsonBoolMember reads one member of a decoded body as a bool. Absent, null
// or a non-bool member reads as false — no claim, not a false claim. The
// sibling of jsonStringMember, and used for exactly one field: `stream`.
func jsonBoolMember(raw map[string]json.RawMessage, name string) bool {
	v, ok := raw[name]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}
