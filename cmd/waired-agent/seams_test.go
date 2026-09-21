package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// TestMain seals the package-global seams that would otherwise reach off
// the machine (CLAUDE.md §Test discipline: seal machine-global state in
// TestMain, not per test).
//
// tagSizeFn is the one that matters here: runPullJob asks it for the
// pull's whole size before starting, and its real implementation is an
// HTTPS request to registry.ollama.ai. Left unsealed, every pull test in
// this package would make one — against a tag like "a:q4" that exists
// only in a fixture — so a clean CI runner would pay a DNS round trip and
// a 404 per test, and a runner with no egress would pay the timeout. The
// sealed default refuses, which is the arm the product must survive: a
// pull whose size is unknown still downloads, with a bar that adds itself
// up as it goes.
func TestMain(m *testing.M) {
	tagSizeFn = func(context.Context, string) (int64, error) {
		return 0, errors.New("tagSizeFn: sealed in TestMain; swap it in the test that wants a size")
	}
	// The disk check before a pull reads this machine's free space; a
	// nearly full runner must not fail pull tests that are about something
	// else.
	freeDiskFn = func(string) (int64, error) { return 1 << 50, nil }
	// The digest read is the same kind of request, for a pinned tag.
	tagDigestFn = func(context.Context, string) (string, error) {
		return "", errors.New("tagDigestFn: sealed in TestMain; swap it in the test that wants a digest")
	}
	// The step-down compares seconds recorded on the reference host class
	// (router.FasterCandidate). The fixture models of this package's
	// recommendation tests have none in the shipped store, so they get
	// seconds here, ordered like their weights: heavier is slower. A model
	// not named here reads the shipped store.
	stepDownTurnSpeedFor = func(m catalog.Manifest, v catalog.Variant) (float64, bool) {
		if s, ok := map[string]float64{"heavy": 400, "light": 150, "tiny": 50}[m.ModelID]; ok {
			return s, true
		}
		set, err := catalog.TurnSpeeds()
		if err != nil {
			return 0, false
		}
		s, _, ok := set.For(m, v)
		return s, ok
	}
	os.Exit(m.Run())
}
