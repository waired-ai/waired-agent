package main

import (
	"context"
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
func TestSharingController_StopsBeforeItPersists(t *testing.T) {
	dir := t.TempDir()
	sc := newSharingController(dir, "", state.MeshShareOn, nil)

	var sharingAtStop bool
	sc.SetOnStop(func() { sharingAtStop = sc.IsSharing() })

	// A runtime directory that cannot be written to: the write fails,
	// and the stop must already have happened.
	if err := os.MkdirAll(dir+"/runtime", 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir+"/runtime", 0o700) })

	err := sc.Unshare(context.Background())
	if os.Geteuid() == 0 {
		t.Skip("running as root: the unwritable directory does not fail the write")
	}
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if sc.IsSharing() {
		t.Error("the computer is still sharing after a failed write")
	}
	if sharingAtStop {
		t.Error("the abort ran before the live flag was cleared")
	}
}

// Turning sharing ON is the mirror image: persist first, so a machine
// that cannot record the choice does not start serving on the strength
// of it.
func TestSharingController_StartPersistsFirst(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the unwritable directory does not fail the write")
	}
	dir := t.TempDir()
	sc := newSharingController(dir, state.SharingOff, state.MeshShareOn, nil)
	if err := os.MkdirAll(dir+"/runtime", 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir+"/runtime", 0o700) })

	if err := sc.Share(context.Background()); err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if sc.IsSharing() {
		t.Error("the computer started sharing on the strength of a write that failed")
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
