package main

import (
	"context"
	"errors"
	"os"
	"testing"
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
	os.Exit(m.Run())
}
