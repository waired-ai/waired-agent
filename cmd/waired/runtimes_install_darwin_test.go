//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// withSeam swaps installOllamaApp for the duration of a test.
func withSeam(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	prev := installOllamaApp
	installOllamaApp = fn
	t.Cleanup(func() { installOllamaApp = prev })
}

// TestInstallOllama_SkipsWhenResolvable: a pre-existing ollama (here via
// the WAIRED_OLLAMA_BINARY override that ResolveBinary honours) means
// installOllama must NOT download anything.
func TestInstallOllama_SkipsWhenResolvable(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "ollama")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAIRED_OLLAMA_BINARY", stub)

	withSeam(t, func(context.Context) error {
		t.Fatal("installOllamaApp must not run when ollama is already resolvable")
		return nil
	})
	if err := installOllama(true, t.TempDir()); err != nil {
		t.Fatalf("installOllama: %v", err)
	}
}

// TestInstallOllama_RunsWhenAbsent: with nothing resolvable, the seam is
// invoked (yes=true skips the TTY confirm).
func TestInstallOllama_RunsWhenAbsent(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	if _, err := download.ResolveBinary(""); err == nil {
		t.Skip("host has a resolvable ollama at a well-known path; cannot exercise the install branch")
	}

	called := false
	withSeam(t, func(context.Context) error { called = true; return nil })
	if err := installOllama(true, t.TempDir()); err != nil {
		t.Fatalf("installOllama: %v", err)
	}
	if !called {
		t.Error("installOllamaApp was not invoked")
	}
}

// TestInstallOllama_PropagatesError: a seam failure surfaces wrapped.
func TestInstallOllama_PropagatesError(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	if _, err := download.ResolveBinary(""); err == nil {
		t.Skip("host has a resolvable ollama; cannot exercise the install branch")
	}

	sentinel := errors.New("boom")
	withSeam(t, func(context.Context) error { return sentinel })
	err := installOllama(true, t.TempDir())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
}

func TestOllamaDarwinURL_EnvOverride(t *testing.T) {
	t.Setenv("WAIRED_OLLAMA_DARWIN_URL", "https://example.invalid/Ollama-darwin.zip")
	if got := ollamaDarwinURL(); got != "https://example.invalid/Ollama-darwin.zip" {
		t.Errorf("ollamaDarwinURL() = %q, want the override", got)
	}
	t.Setenv("WAIRED_OLLAMA_DARWIN_URL", "")
	if got := ollamaDarwinURL(); got != ollamaDarwinDefaultURL {
		t.Errorf("ollamaDarwinURL() = %q, want default %q", got, ollamaDarwinDefaultURL)
	}
}

// lowerZipFloor shrinks the size sanity floor for tests that serve tiny
// fake archives.
func lowerZipFloor(t *testing.T, n int64) {
	t.Helper()
	orig := ollamaZipMinBytes
	ollamaZipMinBytes = n
	t.Cleanup(func() { ollamaZipMinBytes = orig })
}

// downloadOllamaZip must land every body byte in dest and stream byte
// progress (Completed/Total from Content-Length) stamped as the
// "download" stage — the silence #615 fixes.
func TestDownloadOllamaZip_StreamsProgress(t *testing.T) {
	lowerZipFloor(t, 4)
	body := bytes.Repeat([]byte("q"), 256<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "Ollama-darwin.zip")
	var events []infruntime.OllamaInstallProgress
	err := downloadOllamaZip(context.Background(), srv.URL, dest, func(p infruntime.OllamaInstallProgress) {
		events = append(events, p)
	})
	if err != nil {
		t.Fatalf("downloadOllamaZip: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("dest = %d bytes (err=%v), want the %d-byte body", len(got), err, len(body))
	}
	if len(events) == 0 {
		t.Fatal("no progress emitted")
	}
	last := events[len(events)-1]
	if last.Stage != "download" || last.Completed != int64(len(body)) || last.Total != int64(len(body)) {
		t.Errorf("final event = %+v, want stage download, completed == total == %d", last, len(body))
	}
}

// A response below the sanity floor is an error page / partial download,
// not a release — refuse it (mirrors the Linux tarball floor).
func TestDownloadOllamaZip_TooSmall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not a zip</html>"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "Ollama-darwin.zip")
	err := downloadOllamaZip(context.Background(), srv.URL, dest, func(infruntime.OllamaInstallProgress) {})
	if err == nil || !strings.Contains(err.Error(), "suspiciously small") {
		t.Fatalf("err = %v, want the size-floor refusal", err)
	}
}

// A non-200 response must surface as an error.
func TestDownloadOllamaZip_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "Ollama-darwin.zip")
	err := downloadOllamaZip(context.Background(), srv.URL, dest, func(infruntime.OllamaInstallProgress) {})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v, want an HTTP 404 error", err)
	}
}
