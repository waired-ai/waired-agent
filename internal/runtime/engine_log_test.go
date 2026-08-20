package runtime

import (
	"strings"
	"testing"
	"time"
)

// A banner written by engineLogSpawnBannerLine is found by
// LastEngineLogSpawn. The two are one format contract
// (waired-ai/waired-agent#878) and nothing else enforces it: the writer
// lives in the linux-only vLLM adapter and the readers live in
// cmd/waired-agent, so a change to one compiles fine against the other
// and simply stops scoping.
func TestEngineLogSpawnBanner_RoundTrips(t *testing.T) {
	line := engineLogSpawnBannerLine(time.Date(2026, 8, 21, 4, 5, 6, 0, time.UTC))
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("banner line = %q, want a trailing newline so the engine's first line starts clean", line)
	}
	if !strings.Contains(line, "2026-08-21T04:05:06Z") {
		t.Errorf("banner line = %q, want the spawn time — it is how a reader tells three attempts apart", line)
	}
	log := "first spawn output\n" + line + "second spawn output\n"
	got := LastEngineLogSpawn(log)
	if strings.Contains(got, "first spawn output") {
		t.Errorf("LastEngineLogSpawn kept the earlier spawn: %q", got)
	}
	if !strings.Contains(got, "second spawn output") {
		t.Errorf("LastEngineLogSpawn dropped the current spawn: %q", got)
	}
}

func TestLastEngineLogSpawn(t *testing.T) {
	banner := func(n string) string {
		return engineLogSpawnBannerLine(time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)) + n + "\n"
	}
	tests := []struct {
		name string
		log  string
		want string
	}{
		{
			// A log written by an agent older than #878, or any ollama
			// engine.log: it holds exactly one spawn, so the whole file
			// IS the last spawn. Returning "" here would blind every
			// reader on a host that has not restarted since upgrading.
			name: "no banner returns the whole log",
			log:  "GPU KV cache size: 1,000 tokens\n",
			want: "GPU KV cache size: 1,000 tokens\n",
		},
		{
			name: "empty",
			log:  "",
			want: "",
		},
		{
			name: "one spawn",
			log:  banner("only"),
			want: banner("only"),
		},
		{
			name: "three attempts keeps the last",
			log:  "preamble\n" + banner("one") + banner("two") + banner("three"),
			want: banner("three"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LastEngineLogSpawn(tc.log); got != tc.want {
				t.Errorf("LastEngineLogSpawn(%q) = %q, want %q", tc.log, got, tc.want)
			}
		})
	}
}
