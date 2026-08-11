package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// The measurement lifecycle, pinned as the product contract from the
// 2026-08-08 owner rulings on #568: once per install (AgentVersion keys
// the reuse), taken only while nothing is serving, floored at 1 so a
// successful probe can never read as "unavailable", and the env seam
// wins without persisting.
func TestEnsureHostMemoryMeasured(t *testing.T) {
	fixedNow := func() time.Time { return time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC) }
	noEnv := func(string) string { return "" }
	quiet := func() bool { return false }
	ramOK := func(context.Context) (int, int, error) { return 16, 6, nil }

	t.Run("fresh host measures and persists", func(t *testing.T) {
		dir := t.TempDir()
		got, err := ensureHostMemoryMeasured(dir, "1.0.0", noEnv, ramOK, quiet, fixedNow)
		if err != nil || got != 6 {
			t.Fatalf("got %d err %v, want 6 nil", got, err)
		}
		rec, _ := state.ReadHostMemory(dir)
		want := state.HostMemoryRecord{AvailableGB: 6, MeasuredAt: "2026-08-09T00:00:00Z", AgentVersion: "1.0.0"}
		if rec != want {
			t.Errorf("persisted %+v, want %+v", rec, want)
		}
	})

	t.Run("same version reuses without probing", func(t *testing.T) {
		dir := t.TempDir()
		must(t, state.WriteHostMemory(dir, state.HostMemoryRecord{AvailableGB: 9, AgentVersion: "1.0.0"}))
		probed := false
		ram := func(context.Context) (int, int, error) { probed = true; return 16, 3, nil }
		got, err := ensureHostMemoryMeasured(dir, "1.0.0", noEnv, ram, quiet, fixedNow)
		if err != nil || got != 9 || probed {
			t.Fatalf("got %d err %v probed %v, want 9 nil false", got, err, probed)
		}
	})

	t.Run("upgrade re-measures", func(t *testing.T) {
		dir := t.TempDir()
		must(t, state.WriteHostMemory(dir, state.HostMemoryRecord{AvailableGB: 9, AgentVersion: "1.0.0"}))
		got, err := ensureHostMemoryMeasured(dir, "1.1.0", noEnv, ramOK, quiet, fixedNow)
		if err != nil || got != 6 {
			t.Fatalf("got %d err %v, want 6 nil", got, err)
		}
		rec, _ := state.ReadHostMemory(dir)
		if rec.AgentVersion != "1.1.0" || rec.AvailableGB != 6 {
			t.Errorf("persisted %+v, want re-measured under 1.1.0", rec)
		}
	})

	t.Run("a serving engine defers the re-measure and keeps the record", func(t *testing.T) {
		dir := t.TempDir()
		must(t, state.WriteHostMemory(dir, state.HostMemoryRecord{AvailableGB: 9, AgentVersion: "1.0.0"}))
		busy := func() bool { return true }
		got, err := ensureHostMemoryMeasured(dir, "1.1.0", noEnv, ramOK, busy, fixedNow)
		if err != nil || got != 9 {
			t.Fatalf("got %d err %v, want the previous figure 9 and nil", got, err)
		}
		rec, _ := state.ReadHostMemory(dir)
		if rec.AgentVersion != "1.0.0" {
			t.Errorf("record was rewritten beside a serving engine: %+v", rec)
		}
	})

	t.Run("probe failure reports and leaves the record", func(t *testing.T) {
		dir := t.TempDir()
		ram := func(context.Context) (int, int, error) { return 0, 0, errors.New("no reader") }
		got, err := ensureHostMemoryMeasured(dir, "1.0.0", noEnv, ram, quiet, fixedNow)
		if err == nil || got != 0 {
			t.Fatalf("got %d err %v, want 0 and an error", got, err)
		}
		if rec, _ := state.ReadHostMemory(dir); rec != (state.HostMemoryRecord{}) {
			t.Errorf("a failed probe persisted %+v", rec)
		}
	})

	t.Run("a near-exhausted reading floors at 1, never 0", func(t *testing.T) {
		dir := t.TempDir()
		ram := func(context.Context) (int, int, error) { return 16, 0, nil }
		got, err := ensureHostMemoryMeasured(dir, "1.0.0", noEnv, ram, quiet, fixedNow)
		if err != nil || got != 1 {
			t.Fatalf("got %d err %v, want 1 nil — 0 on the wire means unmeasured", got, err)
		}
	})

	t.Run("env seam wins and persists nothing", func(t *testing.T) {
		dir := t.TempDir()
		env := func(k string) string {
			if k == hostMemoryEnvVar {
				return "12"
			}
			return ""
		}
		got, err := ensureHostMemoryMeasured(dir, "1.0.0", env, ramOK, quiet, fixedNow)
		if err != nil || got != 12 {
			t.Fatalf("got %d err %v, want 12 nil", got, err)
		}
		if rec, _ := state.ReadHostMemory(dir); rec != (state.HostMemoryRecord{}) {
			t.Errorf("env seam persisted %+v", rec)
		}
		if got := hostMemoryGB(dir, env); got != 12 {
			t.Errorf("hostMemoryGB with env = %d, want 12", got)
		}
	})
}

func TestHostMemoryGB_ReadsRecord(t *testing.T) {
	dir := t.TempDir()
	noEnv := func(string) string { return "" }
	if got := hostMemoryGB(dir, noEnv); got != 0 {
		t.Fatalf("empty state dir = %d, want 0", got)
	}
	must(t, state.WriteHostMemory(dir, state.HostMemoryRecord{AvailableGB: 7, AgentVersion: "x"}))
	if got := hostMemoryGB(dir, noEnv); got != 7 {
		t.Fatalf("got %d, want 7", got)
	}
	// A non-numeric env value is ignored, not an error.
	badEnv := func(k string) string {
		if k == hostMemoryEnvVar {
			return "lots"
		}
		return ""
	}
	if got := hostMemoryGB(dir, badEnv); got != 7 {
		t.Fatalf("bad env value: got %d, want the record's 7", got)
	}
}

// TestHostMemoryMeasurement_PairsTheValueWithItsDate pins waired-agent#699:
// the figure and the timestamp that dates it are read together, from the
// same source, so they cannot be paired wrongly.
//
// The env-seam row is the one worth having. WAIRED_RAM_AVAILABLE_GB is an
// operator/CI override, not a measurement — there is nothing to date, and
// handing back the record's timestamp would attribute the override to a
// measurement that did not produce it. Empty is the honest answer, and it
// is what the wire already means by "no claim".
func TestHostMemoryMeasurement_PairsTheValueWithItsDate(t *testing.T) {
	const measuredAt = "2026-08-09T16:47:06.123456789Z"
	dir := t.TempDir()
	noEnv := func(string) string { return "" }
	envGB := func(k string) string {
		if k == hostMemoryEnvVar {
			return "12"
		}
		return ""
	}

	// Nothing persisted: no value, no date.
	if gb, at := hostMemoryMeasurement(dir, noEnv); gb != 0 || at != "" {
		t.Fatalf("empty state dir = (%d, %q), want (0, \"\")", gb, at)
	}

	must(t, state.WriteHostMemory(dir, state.HostMemoryRecord{
		AvailableGB: 7, MeasuredAt: measuredAt, AgentVersion: "x",
	}))
	if gb, at := hostMemoryMeasurement(dir, noEnv); gb != 7 || at != measuredAt {
		t.Errorf("record = (%d, %q), want (7, %q)", gb, at, measuredAt)
	}

	// The override supplies a number and NOT a date, even though a dated
	// record sits right there.
	if gb, at := hostMemoryMeasurement(dir, envGB); gb != 12 || at != "" {
		t.Errorf("env seam = (%d, %q), want (12, \"\") — an override is not a measurement", gb, at)
	}

	// A record written by an agent predating the field carries no date,
	// and the value still reads.
	dir2 := t.TempDir()
	must(t, state.WriteHostMemory(dir2, state.HostMemoryRecord{AvailableGB: 41, AgentVersion: "old"}))
	if gb, at := hostMemoryMeasurement(dir2, noEnv); gb != 41 || at != "" {
		t.Errorf("pre-addition record = (%d, %q), want (41, \"\")", gb, at)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
