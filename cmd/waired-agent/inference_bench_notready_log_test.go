package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// benchLogLines runs the boot benchmark against a closed port with the
// given EngineReady answer and returns the JSON log records it emitted.
// The whole record is kept — level included — so "logged at the wrong
// level" and "logged the wrong fields" are both writable failures.
func benchLogLines(t *testing.T, model string) []map[string]any {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("engine was contacted at %s despite EngineReady reporting not-ready", r.URL.Path)
	}))
	port := portFromBenchURL(t, srv.URL)
	srv.Close()

	var buf bytes.Buffer
	RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  port,
		EngineReady: func() (bool, string) { return false, model },
		HTTPClient:  http.DefaultClient,
		Logger:      slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

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
	if len(out) == 0 {
		t.Fatal("the not-ready branch logged nothing")
	}
	return out
}

// PRODUCT CONTRACT (waired-agent#633): the not-ready line does not carry
// an empty model.
//
// EngineReady names a model only when it has one to name — four of its
// five not-ready paths return "" — and on a first install the one that
// fires is "no selection committed yet", because the boot benchmark runs
// while `waired init` is still installing the engine and pulling the
// first model. `model=""` read as "something upstream lost the model id",
// which is what the issue asked to have confirmed either way.
func TestRunBootBenchmark_NotReadyWithNoSelectionOmitsTheModelField(t *testing.T) {
	for _, rec := range benchLogLines(t, "") {
		if v, ok := rec["model"]; ok {
			t.Errorf("record %v carries model=%v; an unset selection must not be logged as an empty model", rec["msg"], v)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#633): a fresh install is not a warning.
// The measurement runs by itself minutes later, notReadyBenchResult
// returns the documented Capacity=1 fail-safe, and the 425 door is
// already a poll-and-retry loop — so this is the expected shape of a
// first install, and a WARN on every one of them is the line that gets
// filtered out, taking the real ones with it (#203/#382 record the same
// lesson for the dial error this branch was added to replace).
func TestRunBootBenchmark_NoSelectionYetIsInfoNotWarn(t *testing.T) {
	for _, rec := range benchLogLines(t, "") {
		if rec["level"] == "WARN" {
			t.Errorf("a first install logged WARN: %v", rec)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#633): a NAMED model whose engine is not
// healthy is a different claim and keeps its WARN — and names the model,
// so the reader knows which one.
func TestRunBootBenchmark_NamedModelWithAnUnhealthyEngineStaysAWarning(t *testing.T) {
	var warned bool
	for _, rec := range benchLogLines(t, "qwen3.5-9b") {
		if rec["level"] != "WARN" {
			continue
		}
		warned = true
		if rec["model"] != "qwen3.5-9b" {
			t.Errorf("WARN record %v does not name the model", rec)
		}
	}
	if !warned {
		t.Error("a named model on an unhealthy engine logged no WARN")
	}
}
