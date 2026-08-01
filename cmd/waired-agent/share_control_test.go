package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestShareController_TransitionsAndPersists(t *testing.T) {
	dir := t.TempDir()
	sc := newShareController(dir, state.ShareMeshShared, nil)
	if !sc.IsShared() {
		t.Fatal("starts shared (true)")
	}
	if sc.IsShareDenied() {
		t.Fatal("IsShareDenied should be false while shared")
	}

	if err := sc.Unshare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sc.IsShared() {
		t.Error("Unshare did not flip flag")
	}
	if !sc.IsShareDenied() {
		t.Error("IsShareDenied should be true while not shared")
	}
	got, err := state.ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != state.ShareMeshNotShared {
		t.Errorf("desired-share after Unshare = %q, want %q", got, state.ShareMeshNotShared)
	}

	if err := sc.Share(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sc.IsShared() {
		t.Error("Share did not flip flag")
	}
	got, _ = state.ReadDesiredShareMesh(dir)
	if got != state.ShareMeshShared {
		t.Errorf("desired-share after Share = %q, want %q", got, state.ShareMeshShared)
	}
}

func TestShareController_StartsFromInitial(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteDesiredShareMesh(dir, state.ShareMeshNotShared); err != nil {
		t.Fatal(err)
	}
	sc := newShareController(dir, state.ShareMeshNotShared, nil)
	if sc.IsShared() {
		t.Fatal("should start not_shared when initial=ShareMeshNotShared")
	}
	cur, desired := sc.State()
	if cur != state.ShareMeshNotShared || desired != state.ShareMeshNotShared {
		t.Errorf("State() = (%q, %q), want (not_shared, not_shared)", cur, desired)
	}
}

// Empty initial value (= "operator has never touched the toggle")
// must boot as shared so a fresh agent with no desired-share file
// keeps Phase 4 / Phase 5 default behaviour.
func TestShareController_EmptyInitialMeansShared(t *testing.T) {
	dir := t.TempDir()
	sc := newShareController(dir, state.ShareMeshState(""), nil)
	if !sc.IsShared() {
		t.Fatal("empty initial state must default to shared (matches Phase 4 default)")
	}
	cur, desired := sc.State()
	if cur != state.ShareMeshShared {
		t.Errorf("current = %q, want shared", cur)
	}
	// No file on disk yet, so desired comes from the live flag.
	if desired != state.ShareMeshShared {
		t.Errorf("desired = %q, want shared (mirrors current when disk is empty)", desired)
	}
}

// --- #316 session suspension (live-only) ---

// TestShareController_SuspendWithholdsWithoutPersisting is the PRODUCT
// CONTRACT the tray's Quit path depends on: peers stop being served
// immediately, but nothing is written to disk, so the operator's sharing
// preference survives. Persisting not_shared here instead would revoke
// that preference for good the first time someone closed the tray.
func TestShareController_SuspendWithholdsWithoutPersisting(t *testing.T) {
	dir := t.TempDir()
	sc := newShareController(dir, state.ShareMeshShared, nil)

	if err := sc.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sc.IsShared() {
		t.Error("suspended agent still reports shared; peers would keep routing to it")
	}
	if !sc.IsShareDenied() {
		t.Error("IsShareDenied must be true while suspended — it gates the peer overlay")
	}
	if !sc.IsSuspended() {
		t.Error("IsSuspended = false after Suspend")
	}
	if got, err := state.ReadDesiredShareMesh(dir); err != nil {
		t.Fatal(err)
	} else if got != "" {
		t.Errorf("desired-share was written (%q); a suspension must persist nothing", got)
	}
	cur, desired := sc.State()
	if cur != state.ShareMeshNotShared {
		t.Errorf("current = %q, want not_shared while suspended", cur)
	}
	if desired != state.ShareMeshShared {
		t.Errorf("desired = %q, want the operator's choice (shared) to survive", desired)
	}

	if err := sc.Unsuspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sc.IsShared() {
		t.Error("Unsuspend did not restore sharing")
	}
}

// A suspension must never turn sharing ON: lifting it returns to the
// persisted choice, which may well be not_shared.
func TestShareController_UnsuspendKeepsOperatorOff(t *testing.T) {
	dir := t.TempDir()
	sc := newShareController(dir, state.ShareMeshNotShared, nil)

	if err := sc.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sc.Unsuspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sc.IsShared() {
		t.Error("Unsuspend turned sharing on; it must only lift the override")
	}
}

// An explicit operator toggle clears the session override, so a stale
// suspension can never swallow the very action the operator just took.
func TestShareController_ExplicitToggleClearsSuspension(t *testing.T) {
	dir := t.TempDir()
	sc := newShareController(dir, state.ShareMeshShared, nil)
	if err := sc.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sc.Share(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sc.IsSuspended() {
		t.Error("Share left the suspension latched")
	}
	if !sc.IsShared() {
		t.Error("Share did not take effect while suspended")
	}
}

func TestShareController_StateReportsDesiredFromDisk(t *testing.T) {
	dir := t.TempDir()
	sc := newShareController(dir, state.ShareMeshShared, nil)
	// Operator wrote a flip but daemon hasn't applied it yet (e.g.,
	// edited the file by hand). State() should surface the disk
	// truth in the desired field while leaving current unchanged.
	if err := state.WriteDesiredShareMesh(dir, state.ShareMeshNotShared); err != nil {
		t.Fatal(err)
	}
	cur, desired := sc.State()
	if cur != state.ShareMeshShared {
		t.Errorf("current = %q, want shared (in-memory hasn't flipped yet)", cur)
	}
	if desired != state.ShareMeshNotShared {
		t.Errorf("desired = %q, want not_shared", desired)
	}
}
