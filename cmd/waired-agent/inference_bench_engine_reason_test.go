package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// The engine's own reason for a failed benchmark
// (waired-ai/waired-agent#552, refs #203).
//
// These tests exist because the absence they cover was expensive:
// waired-ai/waired-agent#552 was investigated across three CI runs and a
// complete 711-line engine.log, and none of them could say why the
// benchmark failed. The daemon logged `err="HTTP 500"`, ollama had
// answered with a sentence, and the status check threw it away.

// TestBenchmark_CarriesTheEngineReason walks the shapes an engine
// actually answers a failure with. Each subtest asserts the reason
// reaches BenchResult.Err, which is the field the WARN line and the
// wizard transcript both read.
func TestBenchmark_CarriesTheEngineReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			// ollama's shape.
			"error is a string",
			`{"error":"model requires more system memory (5.6 GiB) than is available (3.8 GiB)"}`,
			"model requires more system memory",
		},
		{
			// The OpenAI-compat surface's shape.
			"error is an object",
			`{"error":{"message":"llama runner process has terminated","type":"server_error"}}`,
			"llama runner process has terminated",
		},
		{
			// Something else on the port. Still more than a number.
			"not JSON at all",
			"upstream connect error or disconnect/reset before headers",
			"upstream connect error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			got := RunBootBenchmark(context.Background(), BenchDeps{
				EngineKind:  signer.InferenceTypeOllama,
				EnginePort:  portFromBenchURL(t, srv.URL),
				EngineModel: "qwen3.5:4b-q4_K_M",
				Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
			})
			if !got.Failed {
				t.Fatalf("Failed=false over a 500; got %+v", got)
			}
			if !strings.Contains(got.Err, "500") {
				t.Errorf("Err = %q, want the status code in it", got.Err)
			}
			if !strings.Contains(got.Err, tc.want) {
				t.Errorf("Err = %q, want the engine's own reason %q in it — "+
					"a bare status code is what made #552 unreadable", got.Err, tc.want)
			}
			if strings.ContainsAny(got.Err, "\n\r") {
				t.Errorf("Err = %q contains a newline; it reaches a slog attribute "+
					"and the init transcript, and neither survives one legibly", got.Err)
			}
		})
	}
}

// TestBenchmark_SaysSoWhenTheEngineSendsNoReason keeps the empty case
// honest. Reporting a bare number is fine when a bare number is all
// there was; what is not fine is reporting one while a reason existed.
func TestBenchmark_SaysSoWhenTheEngineSendsNoReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	got := RunBootBenchmark(context.Background(), BenchDeps{
		EngineKind:  signer.InferenceTypeOllama,
		EnginePort:  portFromBenchURL(t, srv.URL),
		EngineModel: "qwen3.5:4b-q4_K_M",
		Now:         fakeNow(time.Unix(1_700_000_000, 0), time.Second),
	})
	if !got.Failed {
		t.Fatalf("Failed=false over a 502; got %+v", got)
	}
	if !strings.Contains(got.Err, "502") || !strings.Contains(got.Err, "no reason") {
		t.Errorf("Err = %q, want the status and an explicit statement that the engine sent nothing", got.Err)
	}
}

// TestEngineHTTPError_TruncatesAndDrains: an error page is not a
// sentence, and the body is on a keep-alive connection the next request
// wants back.
func TestEngineHTTPError_TruncatesAndDrains(t *testing.T) {
	huge := strings.Repeat("x", 8*engineErrorBodyLimit)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(huge))
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL) //nolint:noctx // test-local, no cancellation to model
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	got := engineHTTPError(resp).Error()
	if len(got) > engineErrorBodyLimit+64 {
		t.Errorf("error is %d bytes; the body prefix is bounded at %d", len(got), engineErrorBodyLimit)
	}
	if !strings.Contains(got, "500") {
		t.Errorf("error = %q, want the status code in it", got)
	}
}
