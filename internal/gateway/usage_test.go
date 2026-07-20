package gateway

import (
	"strings"
	"testing"
)

// usageSniffer reads the engine's own token counters out of bytes that
// have already been forwarded to the client. It must cope with a stream
// split at arbitrary boundaries, and must stay silent — not guess —
// whenever it cannot read the body.

func feedInChunks(s *usageSniffer, body string, size int) {
	for i := 0; i < len(body); i += size {
		end := i + size
		if end > len(body) {
			end = len(body)
		}
		s.Feed([]byte(body[i:end]))
	}
}

const sseWithUsage = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

func TestUsageSniffer_SSEAcrossWriteBoundaries(t *testing.T) {
	// One byte at a time is the adversarial case: every line, and the
	// JSON inside it, is split many times over.
	for _, chunk := range []int{1, 3, 17, 4096} {
		s := newUsageSniffer("text/event-stream", "")
		feedInChunks(s, sseWithUsage, chunk)
		in, out, ok := s.Usage()
		if !ok {
			t.Fatalf("chunk=%d: usage not observed", chunk)
		}
		if in != 11 || out != 7 {
			t.Fatalf("chunk=%d: in=%d out=%d, want 11/7", chunk, in, out)
		}
	}
}

func TestUsageSniffer_SSEWithoutUsage(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "")
	feedInChunks(s, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n", 8)
	if _, _, ok := s.Usage(); ok {
		t.Fatal("reported usage the engine never sent")
	}
}

// A client disconnecting mid-stream leaves the final usage chunk
// unsent; whatever arrived before that point still counts.
func TestUsageSniffer_TruncatedStream(t *testing.T) {
	s := newUsageSniffer("text/event-stream", "")
	cut := strings.Index(sseWithUsage, "data: [DONE]")
	feedInChunks(s, sseWithUsage[:cut], 5)
	in, out, ok := s.Usage()
	if !ok || in != 11 || out != 7 {
		t.Fatalf("in=%d out=%d ok=%v — a usage chunk that did arrive must still count", in, out, ok)
	}
}

func TestUsageSniffer_NonStreamJSON(t *testing.T) {
	body := `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	s := newUsageSniffer("application/json", "")
	feedInChunks(s, body, 7)
	in, out, ok := s.Usage()
	if !ok || in != 5 || out != 9 {
		t.Fatalf("in=%d out=%d ok=%v, want 5/9", in, out, ok)
	}
}

func TestUsageSniffer_SilentWhenUnreadable(t *testing.T) {
	t.Run("compressed body", func(t *testing.T) {
		// Sniffing gzip would mean dropping Accept-Encoding on the
		// engine-facing request, changing behaviour on three surfaces
		// that do not meter at all. Staying silent is the trade.
		s := newUsageSniffer("application/json", "gzip")
		s.Feed([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
		if _, _, ok := s.Usage(); ok {
			t.Fatal("parsed a compressed body")
		}
	})

	t.Run("identity encoding is fine", func(t *testing.T) {
		s := newUsageSniffer("application/json", "identity")
		s.Feed([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
		if _, _, ok := s.Usage(); !ok {
			t.Fatal("identity encoding disabled the sniffer")
		}
	})

	t.Run("oversized JSON body", func(t *testing.T) {
		s := newUsageSniffer("application/json", "")
		big := strings.Repeat("x", usageSnifferLimit+1)
		s.Feed([]byte(big))
		s.Feed([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
		if _, _, ok := s.Usage(); ok {
			t.Fatal("accumulated past the cap")
		}
	})

	t.Run("SSE line that never ends", func(t *testing.T) {
		s := newUsageSniffer("text/event-stream", "")
		s.Feed([]byte("data: " + strings.Repeat("x", usageSnifferLimit+1)))
		if _, _, ok := s.Usage(); ok {
			t.Fatal("unbounded partial line was parsed")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		s := newUsageSniffer("application/json", "")
		s.Feed([]byte(`{"usage":`))
		if _, _, ok := s.Usage(); ok {
			t.Fatal("parsed a truncated object")
		}
	})

	t.Run("nil sniffer", func(t *testing.T) {
		var s *usageSniffer
		s.Feed([]byte("data: x\n"))
		if _, _, ok := s.Usage(); ok {
			t.Fatal("nil sniffer reported usage")
		}
	})
}
