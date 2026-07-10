package gateway

import (
	"net/http"
	"strings"
	"testing"
)

func TestComputeStickyID_HeaderPreferred(t *testing.T) {
	h := http.Header{}
	h.Set(stickyHeader, "session-abc-123")
	got := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	if got != "session-abc-123" {
		t.Errorf("header ID should win; got %q", got)
	}
}

func TestComputeStickyID_HeaderSanitised(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc def", "abcdef"},
		{"abc\nxyz", "abcxyz"},
		{"a!@#$%^&*()b", "ab"},
		// Verifies length truncation to 32 chars.
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"safe-id_42", "safe-id_42"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			h := http.Header{}
			h.Set(stickyHeader, c.in)
			got := ComputeStickyID(h, nil)
			if got != c.want {
				t.Errorf("sanitise(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestComputeStickyID_BodyHashStability documents the deterministic
// fallback: identical request bodies must produce identical sticky
// IDs across processes, so two requests from the same conversation
// pin to the same peer even without the client header.
func TestComputeStickyID_BodyHashStability(t *testing.T) {
	h := http.Header{}
	body := []byte(`{"messages":[{"role":"system","content":"you are a senior engineer working on the waired repo..."}]}`)
	a := ComputeStickyID(h, body)
	b := ComputeStickyID(h, body)
	if a != b {
		t.Errorf("identical bodies must hash identically; got %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("body-hash ID should be 16 hex chars; got %d (%q)", len(a), a)
	}
	if !isHex(a) {
		t.Errorf("body-hash ID should be hex; got %q", a)
	}
}

// TestComputeStickyID_BodyDifferentBodiesDiverge confirms two
// different prompts produce different IDs — otherwise affinity
// would over-stick conversations together.
func TestComputeStickyID_BodyDifferentBodiesDiverge(t *testing.T) {
	h := http.Header{}
	a := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	b := ComputeStickyID(h, []byte(`{"messages":[{"role":"user","content":"world"}]}`))
	if a == b {
		t.Errorf("different bodies should produce different IDs; both are %q", a)
	}
}

// TestComputeStickyID_PrefixOnlyMatters captures the documented
// design choice: only the first 1 KiB of body contributes to the
// hash, so a long conversation with the same system prompt + first
// user turn keeps the affinity even when later turns vary. Cheap
// at scale because the agent doesn't have to feed multi-MB bodies
// through SHA-256 to decide routing.
func TestComputeStickyID_PrefixOnlyMatters(t *testing.T) {
	h := http.Header{}
	base := strings.Repeat("x", stickyPrefixBytes) // exactly fills the prefix
	a := ComputeStickyID(h, []byte(base+"AAA"))
	b := ComputeStickyID(h, []byte(base+"BBB"))
	if a != b {
		t.Errorf("bytes beyond the prefix should not affect ID; got %q vs %q", a, b)
	}
}

// TestComputeStickyID_EmptyInputs returns empty string when neither
// header nor body offer a signal. The Selector treats "" as "no
// affinity hint" and routes purely by score.
func TestComputeStickyID_EmptyInputs(t *testing.T) {
	h := http.Header{}
	if got := ComputeStickyID(h, nil); got != "" {
		t.Errorf("empty inputs should produce empty ID; got %q", got)
	}
	if got := ComputeStickyID(h, []byte{}); got != "" {
		t.Errorf("zero-length body should produce empty ID; got %q", got)
	}
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
