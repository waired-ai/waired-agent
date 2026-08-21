package main

import (
	"strings"
	"testing"
	"time"
)

// TestFirstTokenLine covers what the line says, and — more importantly —
// what it refuses to say. The PRODUCT CONTRACT (waired-agent#912) is that
// this line states measurements and never a verdict: no "cold", no
// "warm", no threshold. A fixed threshold is wrong on at least one
// reference host, because the 4 B model's WARM first token (1,960 ms) is
// 7.5x slower than the 35 B-A3B's warm first token (259 ms).
func TestFirstTokenLine(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	at := now.Add(-12 * time.Minute).Format(time.RFC3339Nano)

	for _, tc := range []struct {
		name    string
		ms      uint32
		at      string
		best    uint32
		want    string
		wantNot []string
	}{
		{
			name: "a slow reading names the host's own best beside it",
			ms:   35400, at: at, best: 2600,
			want: "35.4s, 12 minutes ago (fastest seen here: 2.6s)",
		},
		{
			name: "with no comparable reading the figure stands alone",
			ms:   35400, at: at,
			want: "35.4s, 12 minutes ago",
		},
		{
			name: "sub-second waits keep their milliseconds",
			ms:   259, at: at, best: 0,
			want: "259ms, 12 minutes ago",
		},
		{
			name: "a sub-second best is not rounded away either",
			ms:   1960, at: at, best: 259,
			want: "2.0s, 12 minutes ago (fastest seen here: 259ms)",
		},
		{
			// A timestamp this build cannot read must drop the clause,
			// not guess an age. Whatever this line says about WHEN has to
			// be true, because `model loaded:` above it is about now.
			name: "an unreadable timestamp drops the age rather than inventing one",
			ms:   35400, at: "not-a-time", best: 2600,
			want: "35.4s (fastest seen here: 2.6s)",
		},
		{
			name: "a future timestamp is clock skew, not a negative age",
			ms:   35400, at: now.Add(time.Hour).Format(time.RFC3339Nano),
			want: "35.4s",
		},
		{
			// Zero means the serving leg could not observe a first token
			// (waired-agent#874). Rendering it as 0ms would say the
			// answer was instant.
			name: "an unobserved wait renders nothing at all",
			ms:   0, at: at, best: 2600,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := firstTokenLine(tc.ms, tc.at, tc.best, now)
			if got != tc.want {
				t.Errorf("firstTokenLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFirstTokenLine_StatesNoVerdict is the contract above as an
// assertion rather than a comment: no wording that judges the number may
// creep into this line, on any input.
func TestFirstTokenLine_StatesNoVerdict(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute).Format(time.RFC3339Nano)
	for _, ms := range []uint32{1, 259, 1960, 2600, 35400, 120000} {
		for _, best := range []uint32{0, 259, 2600} {
			got := strings.ToLower(firstTokenLine(ms, at, best, now))
			for _, banned := range []string{"cold", "warm", "slow", "stale", "cache", "hit", "miss"} {
				if strings.Contains(got, banned) {
					t.Errorf("firstTokenLine(%d, best=%d) = %q, must not judge the reading with %q",
						ms, best, got, banned)
				}
			}
		}
	}
}

// TestStatusPrintsTheFirstTokenLine is the reachability half: the
// rendering being right is worth nothing if the row never reaches the
// screen. It also pins the row's PLACE — directly under `model loaded:`,
// which is the whole of waired-agent#912's complaint, since the two
// halves of the answer used to live in two different commands.
func TestStatusPrintsTheFirstTokenLine(t *testing.T) {
	body := `{
	  "subsystem_state": "ready",
	  "runtimes": {"ollama": {"installed": true, "version": "0.32.13", "state": "ready",
	    "model_resident": true, "model_resident_model": "qwen3:8b-q4_K_M",
	    "model_resident_indefinitely": true}},
	  "models": {"ready": ["qwen3-8b-instruct"]},
	  "first_token": {"ms": 35400, "at": "` + time.Now().Add(-3*time.Minute).UTC().Format(time.RFC3339Nano) + `",
	    "best_ms": 2600, "best_of_samples": 4}
	}`
	out := captureStdout(t, func() { printInferenceSummary([]byte(body)) })

	if !strings.Contains(out, "first token:") {
		t.Fatalf("the first-token row never printed:\n%s", out)
	}
	if !strings.Contains(out, "35.4s") || !strings.Contains(out, "fastest seen here: 2.6s") {
		t.Errorf("the row printed without its figures:\n%s", out)
	}
	loaded := strings.Index(out, "model loaded:")
	first := strings.Index(out, "first token:")
	if loaded < 0 || first < loaded {
		t.Errorf("the first-token row must sit under `model loaded:`, not above it:\n%s", out)
	}
}

// TestStatusOmitsTheFirstTokenRowWhenThereIsNothingToSay: an older daemon
// omits the key, and a daemon with nothing honest to report omits it too.
// Neither may print a placeholder — an empty row here would be read as
// "no wait", which is the opposite of "not measured".
func TestStatusOmitsTheFirstTokenRowWhenThereIsNothingToSay(t *testing.T) {
	const older = `{"subsystem_state":"ready",
	  "runtimes":{"ollama":{"installed":true,"version":"0.32.13","state":"ready"}},
	  "models":{"ready":["qwen3-8b-instruct"]}}`
	if out := captureStdout(t, func() { printInferenceSummary([]byte(older)) }); strings.Contains(out, "first token") {
		t.Errorf("an agent that reports no first token still got a row:\n%s", out)
	}

	const unobserved = `{"subsystem_state":"ready",
	  "runtimes":{"ollama":{"installed":true,"version":"0.32.13","state":"ready"}},
	  "models":{"ready":["qwen3-8b-instruct"]},
	  "first_token":{"ms":0,"at":"2026-08-21T10:00:00Z"}}`
	if out := captureStdout(t, func() { printInferenceSummary([]byte(unobserved)) }); strings.Contains(out, "first token") {
		t.Errorf("an unobserved wait still got a row:\n%s", out)
	}
}
