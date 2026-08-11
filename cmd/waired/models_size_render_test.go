package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestModelsLs_RendersTheSizeTheDaemonReports closes the loop the other
// tests each cover half of: given a management API that reports sizes,
// `waired models ls` prints them, in the units the rest of the CLI uses.
//
// The column read "-" for every row before #661 — including a host with
// 12 GB across three models — because nothing ever wrote the figure.
func TestModelsLs_RendersTheSizeTheDaemonReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[
			{"model_id":"qwen3-8b","state":"ready","size_bytes":5100000000},
			{"model_id":"gemma3-270m","state":"ready","size_bytes":320000000},
			{"model_id":"never-pulled","state":"not_present"}
		]}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runModelsLsBody(srv.URL, false); err != nil {
			t.Fatalf("runModelsLsBody: %v", err)
		}
	})

	if !strings.Contains(out, "5.1 GB") {
		t.Errorf("no size for the 5.1 GB model:\n%s", out)
	}
	// The reason for HumanBytes over a fixed GB divisor: this one read
	// "0.3GB" under the old formatting.
	if !strings.Contains(out, "320.0 MB") {
		t.Errorf("small model not shown in MB:\n%s", out)
	}
	// A model that was never downloaded has no size, and says so rather
	// than claiming zero bytes.
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "never-pulled") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the not-present model is missing from the listing:\n%s", out)
	}
	if !strings.Contains(line, "-") {
		t.Errorf("not-present model should show \"-\", got: %q", line)
	}
}
