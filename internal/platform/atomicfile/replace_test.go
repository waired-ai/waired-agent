package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The retry loop, table-tested below the OS. Every case here runs on all
// three platforms because rename / blocked / sleep are injected — the point
// of putting the seam there.
func TestReplaceWithRetry(t *testing.T) {
	errBlocked := errors.New("a handle is open")
	errFatal := errors.New("no such directory")

	for _, tc := range []struct {
		name       string
		failFirst  int   // how many attempts fail with errBlocked
		failWith   error // what the failing attempts return
		attempts   int
		wantErr    error
		wantCalls  int
		wantSleeps int
	}{
		{
			name:      "succeeds on the first attempt",
			attempts:  40,
			wantCalls: 1,
		},
		{
			name:       "waits out a held destination",
			failFirst:  3,
			failWith:   errBlocked,
			attempts:   40,
			wantCalls:  4,
			wantSleeps: 3,
		},
		{
			name:      "does not retry an error retrying cannot fix",
			failFirst: 99,
			failWith:  errFatal,
			attempts:  40,
			wantErr:   errFatal,
			wantCalls: 1,
		},
		{
			name:       "gives up after the last attempt, returning the real error",
			failFirst:  99,
			failWith:   errBlocked,
			attempts:   3,
			wantErr:    errBlocked,
			wantCalls:  3,
			wantSleeps: 2, // no sleep after the final attempt
		},
		{
			name:      "one attempt means no retry",
			failFirst: 99,
			failWith:  errBlocked,
			attempts:  1,
			wantErr:   errBlocked,
			wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, sleeps := 0, 0
			err := replaceWithRetry(
				func() error {
					calls++
					if calls <= tc.failFirst {
						return tc.failWith
					}
					return nil
				},
				func(err error) bool { return errors.Is(err, errBlocked) },
				func(time.Duration) { sleeps++ },
				tc.attempts,
				time.Hour, // never actually waited: sleep is injected
			)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Errorf("rename calls = %d, want %d", calls, tc.wantCalls)
			}
			if sleeps != tc.wantSleeps {
				t.Errorf("sleeps = %d, want %d", sleeps, tc.wantSleeps)
			}
		})
	}
}

// Replace's own happy path, on a real filesystem, on whatever OS is running
// the test. The Windows-only race this package exists for is in
// replace_windows_test.go.
func TestReplace(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(dst, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, ".state-1")
	if err := os.WriteFile(src, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Replace(src, dst); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Errorf("destination = %q, want %q", got, "after")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("the staged file survived the rename: %v", err)
	}
}

// A failure retrying cannot fix comes straight back, rather than costing the
// whole retry budget first.
func TestReplace_MissingSourceFailsImmediately(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	err := Replace(filepath.Join(dir, "nope"), filepath.Join(dir, "state"))
	if err == nil {
		t.Fatal("Replace of a missing source returned nil")
	}
	if elapsed := time.Since(start); elapsed > replaceAttempts*replacePause {
		t.Errorf("took %s — a non-retryable error spent the retry budget", elapsed)
	}
}
