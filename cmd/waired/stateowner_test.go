package main

import (
	"errors"
	"testing"
)

// TestHandStateToServiceUser_ForwardsDir verifies the helper forwards the
// state dir to the ownership seam.
func TestHandStateToServiceUser_ForwardsDir(t *testing.T) {
	orig := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = orig })

	var got string
	calls := 0
	fixStateOwnership = func(dir string) error {
		calls++
		got = dir
		return nil
	}

	handStateToServiceUser("/var/lib/waired")
	if calls != 1 {
		t.Fatalf("fixStateOwnership called %d times, want 1", calls)
	}
	if got != "/var/lib/waired" {
		t.Errorf("dir = %q, want /var/lib/waired", got)
	}
}

// TestHandStateToServiceUser_SwallowsError verifies the hand-off is
// best-effort: a seam error is warned, not propagated (it has no return
// value and must never abort the calling command).
func TestHandStateToServiceUser_SwallowsError(t *testing.T) {
	orig := fixStateOwnership
	t.Cleanup(func() { fixStateOwnership = orig })

	fixStateOwnership = func(string) error { return errors.New("chown failed") }

	// Must not panic; nothing to assert beyond surviving the call.
	handStateToServiceUser("/var/lib/waired")
}
