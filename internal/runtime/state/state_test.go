package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterPersistsStateAtomically(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, State{
		Phase:        PhaseActive,
		GatewayURL:   "http://127.0.0.1:9473",
		GatewayToken: "token-1",
	})
	if err := w.Set(State{
		Phase:        PhaseActive,
		GatewayURL:   "http://127.0.0.1:9473",
		GatewayToken: "token-1",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Phase != PhaseActive {
		t.Errorf("phase = %q, want %q", got.Phase, PhaseActive)
	}
	if got.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", got.PID, os.Getpid())
	}
	if got.Updated.IsZero() {
		t.Error("Updated should be auto-populated")
	}
	if got.GatewayURL != "http://127.0.0.1:9473" {
		t.Errorf("gateway_url = %q", got.GatewayURL)
	}
	if got.GatewayToken != "token-1" {
		t.Errorf("gateway_token = %q", got.GatewayToken)
	}

	// Second Set must overwrite, not append, and stay valid JSON.
	if err := w.SetPhase(PhasePaused); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	got, err = Read(dir)
	if err != nil {
		t.Fatalf("Read after SetPhase: %v", err)
	}
	if got.Phase != PhasePaused {
		t.Errorf("phase after SetPhase = %q, want %q", got.Phase, PhasePaused)
	}
	if got.GatewayURL != "http://127.0.0.1:9473" {
		t.Errorf("gateway_url should survive SetPhase, got %q", got.GatewayURL)
	}
}

func TestWriterHeartbeatBumpsUpdated(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, State{Phase: PhaseActive})

	t0 := time.Now().UTC().Add(-time.Hour)
	if err := w.Set(State{Phase: PhaseActive, Updated: t0}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _ := Read(dir)
	// Set() auto-populates Updated when zero, but here we passed a value.
	if !got.Updated.Equal(t0) {
		t.Fatalf("Updated should be the explicit t0, got %v", got.Updated)
	}

	t1 := time.Now().UTC()
	if err := w.Heartbeat(t1); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	got, _ = Read(dir)
	if !got.Updated.Equal(t1) {
		t.Errorf("Updated after Heartbeat = %v, want %v", got.Updated, t1)
	}
}

func TestWriterRemoveDeletesFile(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, State{Phase: PhaseActive})
	if err := w.Set(State{Phase: PhaseActive}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(StatePath(dir)); err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if err := w.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(StatePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should not exist after Remove: %v", err)
	}
}

func TestReadMissingReturnsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := Read(dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read of missing file = %v, want ErrNotExist", err)
	}
}

func TestStateEffective(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	livePID := os.Getpid()

	cases := []struct {
		name string
		s    State
		want bool
	}{
		{
			name: "active+fresh+alive",
			s:    State{Phase: PhaseActive, PID: livePID, Updated: now.Add(-2 * time.Second)},
			want: true,
		},
		{
			name: "paused",
			s:    State{Phase: PhasePaused, PID: livePID, Updated: now},
			want: false,
		},
		{
			name: "active+stale",
			s:    State{Phase: PhaseActive, PID: livePID, Updated: now.Add(-time.Minute)},
			want: false,
		},
		{
			name: "active+dead-pid",
			s:    State{Phase: PhaseActive, PID: 999999, Updated: now},
			want: false,
		},
		{
			name: "active+zero-pid",
			s:    State{Phase: PhaseActive, PID: 0, Updated: now},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.s.Effective(now, 15*time.Second)
			if got != tc.want {
				t.Errorf("Effective = %v, want %v (state=%+v)", got, tc.want, tc.s)
			}
		})
	}
}

func TestStateJSONShape(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, State{
		Phase:        PhaseActive,
		GatewayURL:   "http://127.0.0.1:9473",
		GatewayToken: "tok",
	})
	if err := w.Set(State{Phase: PhaseActive, GatewayURL: "http://127.0.0.1:9473", GatewayToken: "tok"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	body, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// Must be valid JSON with the documented field names.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("file is not valid JSON: %v\nbody:\n%s", err, body)
	}
	for _, want := range []string{"phase", "pid", "updated", "gateway_url", "gateway_token", "inference_reachable_local"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("JSON missing field %q\nbody:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Error("file should end with newline")
	}
}

func TestStateReason(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	livePID := os.Getpid()

	cases := []struct {
		name       string
		s          *State
		wantOK     bool
		wantReason string
	}{
		{
			name:   "active+fresh+alive",
			s:      &State{Phase: PhaseActive, PID: livePID, Updated: now.Add(-2 * time.Second)},
			wantOK: true,
		},
		{
			name:       "nil-state",
			s:          nil,
			wantReason: ReasonAgentStopped,
		},
		{
			name:       "paused",
			s:          &State{Phase: PhasePaused, PID: livePID, Updated: now},
			wantReason: ReasonAgentPaused,
		},
		{
			name:       "stale-heartbeat",
			s:          &State{Phase: PhaseActive, PID: livePID, Updated: now.Add(-time.Minute)},
			wantReason: ReasonAgentStopped,
		},
		{
			name:       "dead-pid",
			s:          &State{Phase: PhaseActive, PID: 999999, Updated: now},
			wantReason: ReasonAgentStopped,
		},
		{
			name:       "zero-pid",
			s:          &State{Phase: PhaseActive, PID: 0, Updated: now},
			wantReason: ReasonAgentStopped,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, r := tc.s.Reason(now, 15*time.Second)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if r != tc.wantReason {
				t.Errorf("reason = %q, want %q", r, tc.wantReason)
			}
		})
	}
}

func TestSetInferenceReachableLocal(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, State{Phase: PhaseActive})
	if err := w.Set(State{Phase: PhaseActive}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := w.SetInferenceReachableLocal(true); err != nil {
		t.Fatalf("SetInferenceReachableLocal: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.InferenceReachableLocal {
		t.Error("InferenceReachableLocal should be true after Set(true)")
	}
	// Idempotency: setting the same value should not error and should
	// keep the field's value.
	if err := w.SetInferenceReachableLocal(true); err != nil {
		t.Fatalf("SetInferenceReachableLocal idempotent: %v", err)
	}
	got, _ = Read(dir)
	if !got.InferenceReachableLocal {
		t.Error("InferenceReachableLocal should remain true after idempotent set")
	}
}

func TestSetInferenceReachableInMesh(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, State{Phase: PhaseActive})
	if err := w.Set(State{Phase: PhaseActive}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := w.SetInferenceReachableInMesh(true); err != nil {
		t.Fatalf("SetInferenceReachableInMesh: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.InferenceReachableInMesh {
		t.Error("InferenceReachableInMesh should be true after Set(true)")
	}
	// Round-trip the JSON tag — the wrapper reads this field name on
	// every claude invocation, so a tag rename would break silent
	// integration tests.
	raw, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(raw), `"inference_reachable_in_mesh": true`) {
		t.Fatalf("expected inference_reachable_in_mesh tag in serialized state; got %s", raw)
	}
	// Idempotency.
	if err := w.SetInferenceReachableInMesh(true); err != nil {
		t.Fatalf("SetInferenceReachableInMesh idempotent: %v", err)
	}
	if err := w.SetInferenceReachableInMesh(false); err != nil {
		t.Fatalf("SetInferenceReachableInMesh false: %v", err)
	}
	got, _ = Read(dir)
	if got.InferenceReachableInMesh {
		t.Error("InferenceReachableInMesh should be false after Set(false)")
	}
}

func TestStateFilePathLayout(t *testing.T) {
	dir := "/tmp/example-state-dir"
	if got, want := StatePath(dir), filepath.Join(dir, "runtime", "state"); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
	if got, want := DesiredPhasePath(dir), filepath.Join(dir, "runtime", "desired-phase"); got != want {
		t.Errorf("DesiredPhasePath = %q, want %q", got, want)
	}
}
