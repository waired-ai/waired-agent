package hostsfile

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tempManager(t *testing.T, initial string) (*Manager, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts")
	if initial != "" {
		if err := os.WriteFile(p, []byte(initial), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New(p, []string{"api.anthropic.com"}), p
}

func TestAddCreatesBlock(t *testing.T) {
	m, p := tempManager(t, "")
	if err := m.Add(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if !strings.Contains(got, "127.0.0.1 api.anthropic.com") {
		t.Errorf("block missing redirect line:\n%s", got)
	}
	present, _ := m.Present()
	if !present {
		t.Error("Present() = false after Add")
	}
}

func TestAddIsIdempotent(t *testing.T) {
	m, p := tempManager(t, "")
	for i := 0; i < 3; i++ {
		if err := m.Add(); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if n := strings.Count(got, beginMarker); n != 1 {
		t.Errorf("begin marker appears %d times, want 1:\n%s", n, got)
	}
	if n := strings.Count(got, "127.0.0.1 api.anthropic.com"); n != 1 {
		t.Errorf("redirect line appears %d times, want 1", n)
	}
}

// TestReconcileStaleBlock simulates a crashed prior run that left a block
// behind: Add must strip it and leave exactly one block, never two.
func TestReconcileStaleBlock(t *testing.T) {
	stale := "127.0.0.1 localhost\n" + beginMarker + "\n127.0.0.1 api.anthropic.com\n" + endMarker + "\n"
	m, p := tempManager(t, stale)
	if err := m.Add(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), beginMarker); n != 1 {
		t.Errorf("begin marker appears %d times after reconcile, want 1:\n%s", n, string(b))
	}
	if !strings.Contains(string(b), "127.0.0.1 localhost") {
		t.Errorf("reconcile dropped pre-existing content:\n%s", string(b))
	}
}

func TestRemoveStripsBlockPreservingContent(t *testing.T) {
	const pre = "# system hosts\r\n127.0.0.1 localhost\r\n1.2.3.4 example.com\r\n"
	m, p := tempManager(t, pre)
	if err := m.Add(); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if strings.Contains(got, beginMarker) {
		t.Errorf("block still present after Remove:\n%s", got)
	}
	for _, want := range []string{"127.0.0.1 localhost", "1.2.3.4 example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("Remove dropped pre-existing line %q:\n%s", want, got)
		}
	}
	present, _ := m.Present()
	if present {
		t.Error("Present() = true after Remove")
	}
}

func TestRemoveMissingFileIsNoOp(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "nope"), []string{"api.anthropic.com"})
	if err := m.Remove(); err != nil {
		t.Errorf("Remove(missing) = %v, want nil", err)
	}
}

// TestConcurrentAddRemove hammers Add/Remove from many goroutines against the
// same file, then settles with a final Add. On Linux withHostsLock serializes
// the read-modify-write; this asserts the invariants that a torn/interleaved
// write would break — base content is never lost, the redirect block is never
// duplicated, and the file ends in the deterministic post-Add state. It guards
// against regressions in the lock wiring (the production race is two root
// processes — the systemd hosts hooks and the converge .path oneshot — editing
// /etc/hosts at once).
func TestConcurrentAddRemove(t *testing.T) {
	const base = "127.0.0.1 localhost\n::1 localhost\n10.0.0.1 example.internal\n"
	m, p := tempManager(t, base)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				if err := m.Add(); err != nil {
					t.Errorf("Add: %v", err)
					return
				}
				if err := m.Remove(); err != nil {
					t.Errorf("Remove: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Settle to a known state and verify the invariants.
	if err := m.Add(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	if n := strings.Count(got, beginMarker); n != 1 {
		t.Errorf("beginMarker count = %d, want 1 (block duplicated/garbled):\n%s", n, got)
	}
	if n := strings.Count(got, endMarker); n != 1 {
		t.Errorf("endMarker count = %d, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "127.0.0.1 api.anthropic.com") {
		t.Errorf("redirect line missing after settle:\n%s", got)
	}
	for _, line := range []string{"127.0.0.1 localhost", "::1 localhost", "10.0.0.1 example.internal"} {
		if !strings.Contains(got, line) {
			t.Errorf("base line %q lost during concurrent churn:\n%s", line, got)
		}
	}
}
