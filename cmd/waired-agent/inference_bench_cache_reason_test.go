package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// benchLogsFor runs the boot benchmark against a closed port with the
// given deps and returns the JSON log records. The port is closed on
// purpose: every path these tests care about is decided before the engine
// is contacted, and a failing warm-up gets there fastest.
func benchLogsFor(t *testing.T, deps BenchDeps) []map[string]any {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deps.EnginePort = portFromBenchURL(t, srv.URL)
	srv.Close()

	var buf bytes.Buffer
	deps.EngineKind = signer.InferenceTypeOllama
	deps.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	RunBootBenchmark(context.Background(), deps)

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func findBenchLog(recs []map[string]any, msg string) map[string]any {
	for _, r := range recs {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

// PRODUCT CONTRACT (waired-agent#1150): a configured cache that cannot be
// used says so, with the reason.
//
// benchCacheKey answers "" for three separate reasons, and both guards
// that read it skip without output — so a host that has never cached a
// measurement and one whose cache simply was not reached this boot leave
// identical journals. That is what made #1150 take a session of log
// archaeology to diagnose on live hardware.
func TestRunBootBenchmark_SaysWhyCachingIsOff(t *testing.T) {
	cache := newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil)
	recs := benchLogsFor(t, BenchDeps{
		EngineModel:   "qwen3:8b",
		GPUModel:      "RTX 4090",
		EngineVersion: "0.33.2",
		// No VariantSHA: the case a host serving a model this build's
		// bundled catalog does not carry actually lands in.
		Cache: cache,
	})
	rec := findBenchLog(recs, "inference boot benchmark: caching is off")
	if rec == nil {
		t.Fatalf("nothing said why caching was off; records: %v", recs)
	}
	if got, want := rec["reason"], "the active model is not in this build's catalog"; got != want {
		t.Errorf("reason = %v, want %q", got, want)
	}
}

// Record of today's behaviour: the explicit-benchmark path passes Cache
// nil by design ("an explicit re-run always re-measures"), so it has no
// key to be missing and must not narrate one. Without this the line would
// fire on every `waired runtimes benchmark` and every setup-wizard run.
func TestRunBootBenchmark_NoCacheConfiguredSaysNothingAboutCaching(t *testing.T) {
	recs := benchLogsFor(t, BenchDeps{EngineModel: "qwen3:8b"})
	if rec := findBenchLog(recs, "inference boot benchmark: caching is off"); rec != nil {
		t.Errorf("a run with no cache configured narrated a disabled cache: %v", rec)
	}
}

// PRODUCT CONTRACT (waired-agent#1150): the depth sweep says it too.
//
// depthBenchCacheKey applies the same three-way rule, and a sweep is
// twenty-five minutes of engine — an uncacheable one costs more than an
// uncacheable boot benchmark, not less. Pinned separately because the two
// call sites are copies: a rule fixed in one place and forgotten in the
// other is how the pair goes stale.
func TestRunDepthBenchmark_SaysWhyCachingIsOff(t *testing.T) {
	f := &fakeDepthEngine{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	RunDepthBenchmark(context.Background(), DepthBenchDeps{
		EnginePort:    portOf(t, srv.URL),
		EngineModel:   "test:tag",
		VariantID:     "mtp-q4",
		ContextLength: 200704,
		KVCacheType:   "q8_0",
		GPUModel:      "RTX 4090",
		EngineVersion: "0.33.2",
		// No VariantSHA, same as the boot case above.
		Cache:      newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil),
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Nonce:      "reason",
	})
	if !strings.Contains(buf.String(), "long-context benchmark: caching is off") {
		t.Fatalf("the depth sweep did not say why caching was off; log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "the active model is not in this build's catalog") {
		t.Errorf("the reason was not carried; log:\n%s", buf.String())
	}
}

// A usable key is silent too — the existing "cache miss; measuring" line
// already covers that case, and a second one beside it would double every
// boot's cache narration.
func TestRunBootBenchmark_UsableKeyDoesNotSayCachingIsOff(t *testing.T) {
	cache := newBenchCache(filepath.Join(t.TempDir(), "bench.json"), nil)
	recs := benchLogsFor(t, BenchDeps{
		EngineModel:   "qwen3:8b",
		GPUModel:      "RTX 4090",
		VariantSHA:    "abc123",
		EngineVersion: "0.33.2",
		Cache:         cache,
	})
	if rec := findBenchLog(recs, "inference boot benchmark: caching is off"); rec != nil {
		t.Errorf("a usable cache key narrated a disabled cache: %v", rec)
	}
	if rec := findBenchLog(recs, "inference boot benchmark: cache miss; measuring"); rec == nil {
		t.Errorf("the existing cache-miss line went missing; records: %v", recs)
	}
}
