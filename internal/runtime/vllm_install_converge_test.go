//go:build linux

package runtime

// The two pieces of the converge that touch the venv on disk (#843): the
// pin record it decides from, and the prune that keeps each pin move
// from leaving another ~6 GB behind. The fakes (scriptedRunner, fakeNow)
// are the ones vllm_install_test.go already uses.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newRecordingInstaller wires an installer whose subprocesses are
// scripted, so a test can build several "venvs" in a temp dir.
func newRecordingInstaller(t *testing.T, baseDir string) *VLLMInstaller {
	t.Helper()
	uvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uvDir, "uv"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &VLLMInstaller{
		BaseDir: baseDir,
		UV:      &UVResolver{BinDir: uvDir},
		Runner:  &scriptedRunner{respond: func(scriptedCall) ([]string, error) { return nil, nil }},
		Now:     fakeNow,
	}
}

// The record the converge reads. Without it a host whose hf_transfer /
// transformers / interpreter pin moved on its own looks up to date,
// because the version directory is named after the vLLM release and that
// did not move.
func TestVLLMInstall_RecordsThePinSetBesideTheVenv(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, ok := inst.ActivePins()
	if !ok {
		t.Fatal("ActivePins() = false after an install: the converge would read this venv as predating the record")
	}
	want := VLLMPinSet{
		VLLM:         "0.11.0",
		HFTransfer:   HFTransferPinnedVersion,
		Transformers: TransformersConstraint,
		Python:       VLLMPythonVersion,
	}
	if got != want {
		t.Errorf("ActivePins() = %+v, want %+v", got, want)
	}
	// Inside the VERSION directory, so it cannot outlive the venv it
	// describes: Uninstall and the rollback both remove that directory
	// whole.
	if _, err := os.Stat(filepath.Join(dir, "0.11.0", vllmPinsFile)); err != nil {
		t.Errorf("record is not beside the venv: %v", err)
	}
	if err := inst.Uninstall(context.Background(), "0.11.0"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, ok := inst.ActivePins(); ok {
		t.Error("the record outlived the venv it describes")
	}
}

// A venv installed before this shipped has no record. That is not drift,
// and it must read as "no record" rather than as a zero-valued pin set
// that differs from every pin and rebuilds ~6 GB on every update.
func TestVLLMActivePins_MissingRecordIsNotAnEmptySet(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "0.11.0", vllmPinsFile)); err != nil {
		t.Fatal(err)
	}
	if set, ok := inst.ActivePins(); ok {
		t.Errorf("ActivePins() = %+v, true; want ok=false when there is no record", set)
	}
	// And the policy that reads it leaves such a host alone.
	got := DecideVLLMConverge(VLLMConvergeFacts{
		Installed: true, Version: VLLMPinnedVersion, HasRecord: false, Want: WantedVLLMPins(),
	})
	if got.Install {
		t.Errorf("a venv at the pin with no record must not be rebuilt (reason: %s)", got.Reason)
	}
}

// Pruning is what keeps a converge from leaving another ~6 GB behind on
// every pin move — and what must not take the venv in use, the shared
// interpreter tree, or a directory somebody kept on purpose.
func TestVLLMPrune_RemovesSupersededVenvsOnly(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	for _, v := range []string{"0.11.0", "0.12.0"} {
		if _, err := inst.Install(context.Background(), InstallOpts{Version: v}, nil); err != nil {
			t.Fatalf("Install %s: %v", v, err)
		}
	}
	// The uv-managed interpreter the venvs SYMLINK into (#778). It has
	// no .venv, and removing it would break the venv being kept.
	if err := os.MkdirAll(filepath.Join(dir, "python", "cpython-3.12", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A half-built venv retained by InstallOpts.KeepFailed. maybeRollback
	// renames the whole directory, .venv included, so it is shaped
	// exactly like a version directory and can only be told apart by
	// name.
	failed := filepath.Join(dir, "0.10.0.failed-20260101-000000")
	if err := os.MkdirAll(filepath.Join(failed, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := inst.PruneOtherVersions()
	if err != nil {
		t.Fatalf("PruneOtherVersions: %v", err)
	}
	if len(removed) != 1 || removed[0] != "0.11.0" {
		t.Errorf("removed = %v, want exactly [0.11.0]", removed)
	}
	for _, keep := range []string{"0.12.0", "python", filepath.Base(failed)} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "0.11.0")); !os.IsNotExist(err) {
		t.Errorf("the superseded venv is still on disk: %v", err)
	}
	// The active venv still resolves afterwards — a host that keeps
	// serving is the whole point of installing beside rather than over.
	if active, ok := inst.Active(); !ok || active.Version != "0.12.0" {
		t.Errorf("Active() = %+v, %v after pruning; want the 0.12.0 venv intact", active, ok)
	}
}

// With nothing active, "everything except the active one" is everything.
// Refuse rather than guess.
func TestVLLMPrune_RefusesWhenNothingIsActive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "0.11.0", ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &VLLMInstaller{BaseDir: dir, UV: NewUVResolver(), Runner: &scriptedRunner{}, Now: fakeNow}
	if _, err := inst.PruneOtherVersions(); err == nil {
		t.Fatal("PruneOtherVersions succeeded with no active install")
	}
	if _, err := os.Stat(filepath.Join(dir, "0.11.0")); err != nil {
		t.Errorf("it removed a venv anyway: %v", err)
	}
}

// uv refuses to create over an existing environment ("A virtual
// environment already exists at ...", exit 2), so re-entering a version
// directory has to state whether the caller wants the environment kept
// or replaced. Found on a real host: the comment in the installer said
// uv exits successfully and gave us idempotency for free, the fake
// runner had always made `uv venv` succeed on an existing directory, and
// every companion-pin converge failed at that line (#843).
func TestVLLMInstall_ReentryKeepsOrReplacesByRequest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		recreate    bool
		wantVenvRun bool
		wantClear   bool
	}{
		{name: "converge reconciles into what is there", recreate: false, wantVenvRun: false},
		{name: "the install verb replaces it", recreate: true, wantVenvRun: true, wantClear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			inst := newRecordingInstaller(t, dir)
			if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
				t.Fatalf("seed install: %v", err)
			}
			r := inst.Runner.(*scriptedRunner)
			r.calls = nil

			if _, err := inst.Install(context.Background(),
				InstallOpts{Version: "0.11.0", Recreate: tc.recreate}, nil); err != nil {
				t.Fatalf("re-entry: %v", err)
			}

			venvRun, clear, pipRun := false, false, false
			for _, c := range r.calls {
				if len(c.args) > 0 && c.args[0] == "venv" {
					venvRun = true
					clear = sliceContains(c.args, "--clear")
				}
				if len(c.args) > 1 && c.args[0] == "pip" && c.args[1] == "install" {
					pipRun = true
				}
			}
			if venvRun != tc.wantVenvRun {
				t.Errorf("`uv venv` run = %v, want %v", venvRun, tc.wantVenvRun)
			}
			if clear != tc.wantClear {
				t.Errorf("--clear passed = %v, want %v", clear, tc.wantClear)
			}
			// Either way the wheels are re-resolved: that IS the
			// reconcile, and it is what a companion-pin move needs.
			if !pipRun {
				t.Error("the wheels were never re-resolved")
			}
		})
	}
}

// The failure that destroyed a working install on a real host: a
// re-entry stopped at `uv venv`, and the rollback then removed the
// version directory — which this call had not created. The machine went
// from "vLLM installed and serving" to a dangling `current` symlink,
// unattended, from a converge whose whole claim is that it never removes
// what is there (#843).
func TestVLLMInstall_AFailedReentryLeavesTheExistingVenvAlone(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	inst.Runner = &scriptedRunner{respond: func(c scriptedCall) ([]string, error) {
		return nil, errors.New("uv: A virtual environment already exists")
	}}
	if _, err := inst.Install(context.Background(),
		InstallOpts{Version: "0.11.0", Recreate: true}, nil); err == nil {
		t.Fatal("the scripted failure did not surface")
	}

	if _, err := os.Stat(filepath.Join(dir, "0.11.0")); err != nil {
		t.Fatalf("the version directory was removed by a call that did not create it: %v", err)
	}
	if _, ok := inst.Active(); !ok {
		t.Error("`current` no longer resolves: the host lost the venv it was serving from")
	}
}

// And the other half of the rule: a directory this call DID create is
// still cleaned up, so a failed first install does not leave a husk for
// the next attempt to trip over.
func TestVLLMInstall_AFailedFirstInstallStillRollsBack(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	inst.Runner = &scriptedRunner{respond: func(scriptedCall) ([]string, error) {
		return nil, errors.New("network down")
	}}
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err == nil {
		t.Fatal("the scripted failure did not surface")
	}
	if _, err := os.Stat(filepath.Join(dir, "0.11.0")); !os.IsNotExist(err) {
		t.Errorf("a half-built version directory was left behind: %v", err)
	}
}

// End to end through the real installer: a venv one pin behind is
// rebuilt, the new one is activated, and the old one is gone. This is
// the shape the real converge takes on a host, with only the
// subprocesses faked.
func TestConvergeVLLM_RebuildsAndReclaimsThroughTheRealInstaller(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.20.0"}, nil); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	decision, err := ConvergeVLLM(context.Background(), VLLMConvergeDeps{
		Active: func() (string, bool) {
			res, ok := inst.Active()
			return res.Version, ok
		},
		Pins:      inst.ActivePins,
		FreeBytes: func() int64 { return 500 << 30 },
		Install: func(ctx context.Context) error {
			_, err := inst.Install(ctx, InstallOpts{}, nil)
			return err
		},
		Prune: inst.PruneOtherVersions,
	})
	if err != nil {
		t.Fatalf("ConvergeVLLM: %v", err)
	}
	if !decision.Install {
		t.Fatalf("no converge decided from 0.20.0 to %s (reason: %s)", VLLMPinnedVersion, decision.Reason)
	}
	if decision.PruneErr != nil {
		t.Errorf("PruneErr = %v", decision.PruneErr)
	}
	active, ok := inst.Active()
	if !ok || active.Version != VLLMPinnedVersion {
		t.Fatalf("Active() = %+v, %v; want the venv at the pin %s", active, ok, VLLMPinnedVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, "0.20.0")); !os.IsNotExist(err) {
		t.Errorf("the superseded 0.20.0 venv was not reclaimed: %v", err)
	}
	// And the host is now settled: a second pass does nothing.
	again, err := ConvergeVLLM(context.Background(), VLLMConvergeDeps{
		Active: func() (string, bool) {
			res, ok := inst.Active()
			return res.Version, ok
		},
		Pins:      inst.ActivePins,
		FreeBytes: func() int64 { return 500 << 30 },
		Install: func(context.Context) error {
			t.Error("converged twice: the second pass rebuilt a venv already at the pin set")
			return nil
		},
		Prune: inst.PruneOtherVersions,
	})
	if err != nil {
		t.Fatalf("second ConvergeVLLM: %v", err)
	}
	if again.Install {
		t.Errorf("second pass decided to install (reason: %s)", again.Reason)
	}
}
