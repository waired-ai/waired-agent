package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildExternalAdapters_EmptyReturnsNil(t *testing.T) {
	if got := buildExternalAdapters(nil, newDiscardLogger()); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := buildExternalAdapters([]agentconfig.ExternalEndpoint{}, newDiscardLogger()); got != nil {
		t.Errorf("expected nil for zero-length input, got %v", got)
	}
}

func TestBuildExternalAdapters_DisabledSkipped(t *testing.T) {
	eps := []agentconfig.ExternalEndpoint{
		{ID: "lan", URL: "http://192.168.1.10:8000/v1", Disable: true},
	}
	if got := buildExternalAdapters(eps, newDiscardLogger()); len(got) != 0 {
		t.Errorf("disabled entry should be skipped; got %d adapter(s)", len(got))
	}
}

func TestBuildExternalAdapters_BuildsEachActiveEntry(t *testing.T) {
	eps := []agentconfig.ExternalEndpoint{
		{ID: "lan", URL: "http://192.168.1.10:8000/v1"},
		{ID: "openai", URL: "https://api.openai.com/v1", AuthEnvVar: "OPENAI_API_KEY"},
		{ID: "dead", URL: "http://disabled:8000", Disable: true},
	}
	got := buildExternalAdapters(eps, newDiscardLogger())
	if len(got) != 2 {
		t.Fatalf("expected 2 adapters, got %d", len(got))
	}
	if want := "openai-compat:lan"; got[0].Name() != want {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name(), want)
	}
	if want := "openai-compat:openai"; got[1].Name() != want {
		t.Errorf("got[1].Name = %q, want %q", got[1].Name(), want)
	}
}

func TestBuildExternalAdapters_MalformedSkipped(t *testing.T) {
	// agentconfig.Validate catches this at boot, but buildExternalAdapters
	// is defensive — a malformed entry must not break the others.
	eps := []agentconfig.ExternalEndpoint{
		{ID: "bad", URL: ""},
		{ID: "good", URL: "http://192.168.1.10:8000"},
	}
	got := buildExternalAdapters(eps, newDiscardLogger())
	if len(got) != 1 {
		t.Fatalf("expected 1 adapter (bad entry skipped), got %d", len(got))
	}
	if !strings.HasPrefix(got[0].Name(), "openai-compat:good") {
		t.Errorf("good entry should survive; got %q", got[0].Name())
	}
}
