package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// waired#1297, owner ruling 2026-08-30. The machine keeps one sharing
// answer and the control plane keeps the rest. Everything below is a
// PRODUCT CONTRACT from that ruling, not a record of today's behaviour.

func TestSharingController_DefaultsToSharing(t *testing.T) {
	// A computer nobody has configured shares, and a control plane that
	// has said nothing leaves it in its own mesh. Defaulting either way
	// to "off" would take a fleet out of service on upgrade.
	sc := newSharingController(t.TempDir(), "", "", nil)
	if !sc.IsSharing() {
		t.Error("a computer with no persisted choice is not sharing")
	}
	if !sc.IsShared() {
		t.Error("a computer with no instruction was withheld from its own mesh")
	}
	if sc.IsShareDenied() {
		t.Error("IsShareDenied disagrees with IsShared")
	}
}

func TestSharingController_HardKillStopsEverything(t *testing.T) {
	dir := t.TempDir()
	sc := newSharingController(dir, "", state.MeshShareOn, nil)
	var stops int
	sc.SetOnStop(func() { stops++ })

	if err := sc.Unshare(context.Background()); err != nil {
		t.Fatalf("Unshare: %v", err)
	}
	if sc.IsSharing() || sc.IsShared() {
		t.Error("the machine still reports as sharing after the hard kill")
	}
	if stops != 1 {
		t.Errorf("the stop hook ran %d times, want 1", stops)
	}
	// The console's setting is untouched: it is not this switch's to
	// change, and the computer coming back must find it where the owner
	// left it.
	if sc.MeshShare() != state.MeshShareOn {
		t.Errorf("MeshShare = %q, want the control plane's value untouched", sc.MeshShare())
	}
	if got, _ := state.ReadDesiredSharing(dir); got != state.SharingOff {
		t.Errorf("persisted = %q, want %q", got, state.SharingOff)
	}

	if err := sc.Share(context.Background()); err != nil {
		t.Fatalf("Share: %v", err)
	}
	if !sc.IsShared() {
		t.Error("turning sharing back on did not restore the console's distribution")
	}
}

// public share spec §8.3: stopping waits for nothing. The live flag and
// the abort come first, persistence after — so a full disk cannot leave
// a computer serving with the gate open.
//
// The write is failed through the controller's own seam rather than by
// making a directory unwritable: os.Chmod does nothing on Windows, and
// the ordering being pinned here is the same on every OS.
func TestSharingController_StopsBeforeItPersists(t *testing.T) {
	sc := newSharingController(t.TempDir(), "", state.MeshShareOn, nil)

	var sharingAtStop, stopped bool
	sc.SetOnStop(func() { stopped, sharingAtStop = true, sc.IsSharing() })
	sc.writeFn = func(string, state.SharingState) error {
		return errors.New("disk full")
	}

	err := sc.Unshare(context.Background())
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if sc.IsSharing() {
		t.Error("the computer is still sharing after a failed write")
	}
	if !stopped {
		t.Fatal("the abort never ran")
	}
	if sharingAtStop {
		t.Error("the abort ran before the live flag was cleared")
	}
}

// Turning sharing ON is the mirror image: persist first, so a machine
// that cannot record the choice does not start serving on the strength
// of it.
func TestSharingController_StartPersistsFirst(t *testing.T) {
	sc := newSharingController(t.TempDir(), state.SharingOff, state.MeshShareOn, nil)
	sc.writeFn = func(string, state.SharingState) error {
		return errors.New("disk full")
	}
	if err := sc.Share(context.Background()); err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if sc.IsSharing() {
		t.Error("the computer started sharing on the strength of a write that failed")
	}
}

// And the seam carries the real arguments, so the failing case above is
// about the ordering rather than about a fake that took none.
func TestSharingController_PersistsThroughTheSeam(t *testing.T) {
	dir := t.TempDir()
	sc := newSharingController(dir, "", state.MeshShareOn, nil)
	var gotDir string
	var gotValue state.SharingState
	sc.writeFn = func(d string, v state.SharingState) error {
		gotDir, gotValue = d, v
		return nil
	}
	if err := sc.Unshare(context.Background()); err != nil {
		t.Fatalf("Unshare: %v", err)
	}
	if gotDir != dir || gotValue != state.SharingOff {
		t.Errorf("seam saw (%q, %q), want (%q, %q)", gotDir, gotValue, dir, state.SharingOff)
	}
}

// The session latch (#316) is not a policy: quitting the app stops
// serving without touching what the operator chose, and a daemon restart
// clears it by construction. Since waired#1297 it covers public serving
// too, which is why the stop hook fires for it.
func TestSharingController_SuspendIsNotAPolicy(t *testing.T) {
	dir := t.TempDir()
	sc := newSharingController(dir, "", state.MeshShareOn, nil)
	var stops int
	sc.SetOnStop(func() { stops++ })

	if err := sc.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if sc.IsSharing() || sc.IsShared() {
		t.Error("a suspended computer still reports as sharing")
	}
	if stops != 1 {
		t.Errorf("the stop hook ran %d times on suspend, want 1", stops)
	}
	if _, err := os.Stat(state.DesiredSharingPath(dir)); !os.IsNotExist(err) {
		t.Errorf("suspend wrote to disk: %v", err)
	}
	cur, desired := sc.State()
	if cur != state.SharingOff || desired != state.SharingOn {
		t.Errorf("State() = (%q, %q), want (off, on): the operator's choice must survive", cur, desired)
	}

	if err := sc.Unsuspend(context.Background()); err != nil {
		t.Fatalf("Unsuspend: %v", err)
	}
	if !sc.IsSharing() {
		t.Error("lifting the latch did not restore sharing")
	}
}

// Unsuspend lifts the latch and nothing else: an operator who turned
// sharing off stays off when the app starts.
func TestSharingController_UnsuspendDoesNotTurnSharingOn(t *testing.T) {
	sc := newSharingController(t.TempDir(), state.SharingOff, state.MeshShareOn, nil)
	if err := sc.Unsuspend(context.Background()); err != nil {
		t.Fatalf("Unsuspend: %v", err)
	}
	if sc.IsSharing() {
		t.Error("lifting the latch turned sharing on")
	}
}

// The control plane's mesh setting is applied, cached so a restart does
// not open a gap, and never written by anything here (waired-agent#646).
func TestSharingController_MeshShareFromControlPlane(t *testing.T) {
	dir := t.TempDir()
	sc := newSharingController(dir, "", "", nil)

	sc.SetMeshShareFromControlPlane(state.MeshShareOff)
	if sc.IsShared() {
		t.Error("the mesh gate stayed open after the console closed it")
	}
	// The machine is still lending itself out — the two are different
	// questions, and public serving does not stop because the owner took
	// this computer out of their own mesh.
	if !sc.IsSharing() {
		t.Error("a console setting turned off the machine's own switch")
	}
	if got, _ := state.ReadAppliedMeshShare(dir); got != state.MeshShareOff {
		t.Errorf("cached = %q, want %q", got, state.MeshShareOff)
	}

	// A value this build does not know is left un-acted, so a newer
	// control-plane vocabulary stays pending across an upgrade rather
	// than being misapplied.
	sc.SetMeshShareFromControlPlane(state.MeshShareState("sometimes"))
	if sc.MeshShare() != state.MeshShareOff {
		t.Errorf("an unknown instruction changed the setting: %q", sc.MeshShare())
	}

	sc.SetMeshShareFromControlPlane(state.MeshShareOn)
	if !sc.IsShared() {
		t.Error("the mesh gate stayed closed after the console reopened it")
	}
}

// A restart starts from the cache rather than from "share with
// everybody": the console's answer has not changed, and re-deriving it
// from the default would serve the owner's own mesh for a poll interval
// against their setting.
func TestSharingController_BootsFromTheCachedInstruction(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteAppliedMeshShare(dir, state.MeshShareOff); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mesh, err := state.ReadAppliedMeshShare(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sc := newSharingController(dir, "", mesh, nil)
	if sc.IsShared() {
		t.Error("a restart reopened the mesh gate the console had closed")
	}
}

// The status surface reads the console's public setting through a seam,
// because the public controller is built after this one. An unwired seam
// reports empty rather than "off": before the first signed map the
// answer is unknown, and reporting it as off would name a choice nobody
// made.
func TestSharingController_PublicReporters(t *testing.T) {
	sc := newSharingController(t.TempDir(), "", "", nil)
	if got := sc.PublicShare(); got != "" {
		t.Errorf("PublicShare with no reporter = %q, want empty", got)
	}
	if got := sc.PublicMaxClients(); got != 0 {
		t.Errorf("PublicMaxClients with no reporter = %d, want 0", got)
	}
	sc.SetPublicReporters(func() bool { return true }, func() int { return 4 })
	if got := sc.PublicShare(); got != state.SharingOn {
		t.Errorf("PublicShare = %q, want %q", got, state.SharingOn)
	}
	if got := sc.PublicMaxClients(); got != 4 {
		t.Errorf("PublicMaxClients = %d, want 4", got)
	}
}

// TestSharingController_MeshShareHasAnUnknownState is the fix for what
// made the last defect invisible (waired#1305).
//
// The control plane sends "on" or "off" to every capable agent —
// foldSelfMeshShare has no third branch — so an empty value means this
// computer has not been told. That is not the same answer as being told
// to share, even though both serve. A boolean could not say it, and the
// mesh-share-v1 defect therefore read as success on real hardware: the
// capability was never declared, the fold never ran, and the agent
// reported its own boot default.
func TestSharingController_MeshShareHasAnUnknownState(t *testing.T) {
	sc := newSharingController(t.TempDir(), "", "", nil)
	if got := sc.MeshShare(); got != "" {
		t.Errorf("MeshShare with no instruction = %q, want the unknown state", got)
	}
	if !sc.IsShared() {
		t.Error("a computer that has not been told was withheld from its own mesh")
	}

	sc.SetMeshShareFromControlPlane(state.MeshShareOn)
	if got := sc.MeshShare(); got != state.MeshShareOn {
		t.Errorf("MeshShare after being told on = %q", got)
	}
	sc.SetMeshShareFromControlPlane(state.MeshShareOff)
	if got := sc.MeshShare(); got != state.MeshShareOff {
		t.Errorf("MeshShare after being told off = %q", got)
	}
	if sc.IsShared() {
		t.Error("an explicit off did not withhold the computer from its own mesh")
	}

	// An unrecognised value is ignored rather than treated as unknown: it
	// comes from a control plane this build does not understand, and
	// forgetting the last real instruction would open serving back up.
	sc.SetMeshShareFromControlPlane(state.MeshShareState("sometimes"))
	if got := sc.MeshShare(); got != state.MeshShareOff {
		t.Errorf("an unrecognised instruction changed the answer to %q", got)
	}
}

// TestSharingController_MeshShareSurvivesARestart pins that the cached
// instruction is what the controller starts from, so a restart does not
// open a gap before the first map frame.
func TestSharingController_MeshShareSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	sc := newSharingController(dir, "", "", nil)
	sc.SetMeshShareFromControlPlane(state.MeshShareOff)

	cached, err := state.ReadAppliedMeshShare(dir)
	if err != nil {
		t.Fatalf("ReadAppliedMeshShare: %v", err)
	}
	again := newSharingController(dir, "", cached, nil)
	if again.MeshShare() != state.MeshShareOff {
		t.Errorf("mesh instruction after a restart = %q, want off", again.MeshShare())
	}
	if again.IsShared() {
		t.Error("a restart put a withheld computer back in its own mesh")
	}
}

// TestSharingController_UnshareNeverReopensTheGate pins the ordering on
// the one path whose contract is to wait for nothing.
//
// Unshare clears the session suspension as well, because an explicit
// operator action should not be swallowed by a stale latch. Doing that
// FIRST flipped IsSharing() from false back to true — the gates read it
// lock-free, per request — and only then stored the operator's off
// (waired#1305). The stop hook runs inside that window, so it is where
// the gate can be observed.
func TestSharingController_UnshareNeverReopensTheGate(t *testing.T) {
	gate := &gateWatcher{}
	sc := newSharingController(t.TempDir(), state.SharingOn, state.MeshShareOn, slog.New(gate))
	gate.sc = sc
	if err := sc.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	gate.arm()

	var sharingAtStop, sharedAtStop bool
	sc.SetOnStop(func() {
		sharingAtStop = sc.IsSharing()
		sharedAtStop = sc.IsShared()
	})
	if err := sc.Unshare(context.Background()); err != nil {
		t.Fatalf("Unshare: %v", err)
	}
	if sharingAtStop || sharedAtStop {
		t.Errorf("the gate was open while stopping: sharing=%v shared=%v", sharingAtStop, sharedAtStop)
	}
	// The stop hook alone cannot see the window: it runs after the flag is
	// stored. The window is between clearing the latch and storing the
	// choice, and what occupies it is a log write — so the logger is where
	// to stand and look. gateWatcher reports the gate at every record.
	if open := gate.sawOpen(); open != "" {
		t.Errorf("the gate was open during Unshare, at %q", open)
	}
	if sc.IsSharing() || sc.IsSuspended() {
		t.Errorf("after Unshare: sharing=%v suspended=%v, want off and not suspended",
			sc.IsSharing(), sc.IsSuspended())
	}
	if _, desired := sc.State(); desired != state.SharingOff {
		t.Errorf("persisted choice = %q, want off", desired)
	}
}

// gateWatcher reads the serving gate at every log record, which is the
// only place inside Unshare a test can stand: the ordering hazard it
// exists for is a window whose whole content is a synchronous log write.
type gateWatcher struct {
	sc     *sharingController
	armed  bool
	opened string
}

func (g *gateWatcher) arm()                                     { g.armed = true }
func (g *gateWatcher) sawOpen() string                          { return g.opened }
func (g *gateWatcher) Enabled(context.Context, slog.Level) bool { return true }
func (g *gateWatcher) WithAttrs([]slog.Attr) slog.Handler       { return g }
func (g *gateWatcher) WithGroup(string) slog.Handler            { return g }

func (g *gateWatcher) Handle(_ context.Context, r slog.Record) error {
	if g.armed && g.opened == "" && g.sc.IsSharing() {
		g.opened = r.Message
	}
	return nil
}
