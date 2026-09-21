//go:build linux

package runtime

// The pieces of the converge that touch the venv on disk (#843): the pin
// record it decides from, the new directory every build goes into, and
// the prune that reclaims what nothing uses (waired-agent#1431). The fakes
// (scriptedRunner, fakeNow) are the ones vllm_install_test.go already uses.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRecordingInstaller wires an installer whose subprocesses are
// scripted, so a test can build several "venvs" in a temp dir.
func newRecordingInstaller(t *testing.T, baseDir string) *VLLMInstaller {
	t.Helper()
	uvDir := t.TempDir()
	if err := os.WriteFile(uvStubPath(t, uvDir), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &VLLMInstaller{
		BaseDir: baseDir,
		UV:      &UVResolver{Root: uvDir},
		Runner:  &scriptedRunner{respond: func(scriptedCall) ([]string, error) { return nil, nil }},
		Now:     fakeNow,
	}
}

// The record the converge reads. Without it a host whose
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
func TestVLLMPruneUnused_RemovesOnlyWhatNothingUses(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	for _, v := range []string{"0.10.0", "0.11.0", "0.12.0"} {
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
	failed := filepath.Join(dir, "0.9.0.failed-20260101-000000")
	if err := os.MkdirAll(filepath.Join(failed, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 0.10.0 is the venv a running engine was started from: superseded
	// twice, and still in use (waired-agent#1431).
	removed, err := inst.PruneUnused(map[string]bool{"0.10.0": true})
	if err != nil {
		t.Fatalf("PruneUnused: %v", err)
	}
	if len(removed) != 1 || removed[0] != "0.11.0" {
		t.Errorf("removed = %v, want exactly [0.11.0]", removed)
	}
	for _, keep := range []string{"0.10.0", "0.12.0", "python", filepath.Base(failed)} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	if active, ok := inst.Active(); !ok || active.Dir != "0.12.0" {
		t.Errorf("Active() = %+v, %v after pruning; want the 0.12.0 venv intact", active, ok)
	}
}

// With nothing active, "everything except the active one" is everything.
// Refuse rather than guess.
func TestVLLMPruneUnused_RefusesWhenNothingIsActive(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "0.11.0", ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &VLLMInstaller{BaseDir: dir, UV: NewUVResolverAt(t.TempDir()), Runner: &scriptedRunner{}, Now: fakeNow}
	if _, err := inst.PruneUnused(nil); err == nil {
		t.Fatal("PruneUnused succeeded with no active install")
	}
	if _, err := os.Stat(filepath.Join(dir, "0.11.0")); err != nil {
		t.Errorf("it removed a venv anyway: %v", err)
	}
}

// A build never goes into a directory that is already there — not the
// same version, not a companion-pin move, not the explicit reinstall. The
// directory there may be the one a running engine was started from, and
// Python loads lazily: wheels replaced or a tree cleared under it fail the
// engine's next request (waired-agent#1431). The second build of 0.11.0
// goes to 0.11.0~2, every subprocess it runs points there, and the first
// is left byte for byte.
func TestVLLMInstall_NeverBuildsIntoAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	sentinel := filepath.Join(dir, "0.11.0", ".venv", "lib", "flashinfer.jinja")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("template the running engine has not read yet"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := inst.Runner.(*scriptedRunner)
	r.calls = nil

	res, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if res.Dir != "0.11.0~2" || res.Version != "0.11.0" {
		t.Fatalf("result = dir %q version %q, want 0.11.0~2 / 0.11.0", res.Dir, res.Version)
	}
	newVenv := filepath.Join(dir, "0.11.0~2", ".venv")
	for _, c := range r.calls {
		joined := c.binary + " " + strings.Join(c.args, " ")
		if strings.Contains(joined, filepath.Join(dir, "0.11.0", ".venv")) {
			t.Errorf("a subprocess touched the existing venv: %s", joined)
		}
		if len(c.args) > 0 && c.args[0] == "venv" {
			if c.args[len(c.args)-1] != newVenv || sliceContains(c.args, "--clear") {
				t.Errorf("uv venv = %v, want a plain create of %s", c.args, newVenv)
			}
		}
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "template the running engine has not read yet" {
		t.Errorf("the existing venv changed under its engine: %q, %v", b, err)
	}
	active, ok := inst.Active()
	if !ok || active.Dir != "0.11.0~2" || active.Version != "0.11.0" {
		t.Errorf("Active() = %+v, %v; want the new directory, version 0.11.0", active, ok)
	}
	if pins, ok := inst.ActivePins(); !ok || pins.VLLM != "0.11.0" {
		t.Errorf("ActivePins() = %+v, %v; want the record beside the new venv", pins, ok)
	}
}

// A failed build removes only the directory it claimed; the venv that was
// active stays active.
func TestVLLMInstall_AFailedRebuildLeavesTheExistingVenvAlone(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	inst.Runner = &scriptedRunner{respond: func(c scriptedCall) ([]string, error) {
		return nil, errors.New("network down")
	}}
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err == nil {
		t.Fatal("the scripted failure did not surface")
	}

	if _, err := os.Stat(filepath.Join(dir, "0.11.0", ".venv", "bin", "python")); err != nil {
		t.Fatalf("the existing venv was touched by a failed rebuild: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0.11.0~2")); !os.IsNotExist(err) {
		t.Errorf("the failed build's own directory was left behind: %v", err)
	}
	if active, ok := inst.Active(); !ok || active.Dir != "0.11.0" {
		t.Errorf("Active() = %+v, %v; the host lost the venv it was serving from", active, ok)
	}
}

// And a directory the build claimed is cleaned up on a failed first
// install, so it does not leave a husk for the next attempt to trip over.
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

func realConvergeDeps(inst *VLLMInstaller) VLLMConvergeDeps {
	return VLLMConvergeDeps{
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
		Lock: func(ctx context.Context) (func(), error) { return inst.Lock(ctx, nil) },
	}
}

// End to end through the real installer: a venv one pin behind is rebuilt
// beside itself and the new one activated. The old one is NOT removed by
// the converge — the engine may be running from it — and goes once the
// daemon's reclaim finds nothing using it (waired-agent#1431).
func TestConvergeVLLM_RebuildsBesideTheVenvInUse(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.20.0"}, nil); err != nil {
		t.Fatalf("seed install: %v", err)
	}

	decision, err := ConvergeVLLM(context.Background(), realConvergeDeps(inst))
	if err != nil {
		t.Fatalf("ConvergeVLLM: %v", err)
	}
	if !decision.Install {
		t.Fatalf("no converge decided from 0.20.0 to %s (reason: %s)", VLLMPinnedVersion, decision.Reason)
	}
	active, ok := inst.Active()
	if !ok || active.Version != VLLMPinnedVersion {
		t.Fatalf("Active() = %+v, %v; want the venv at the pin %s", active, ok, VLLMPinnedVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, "0.20.0", ".venv", "bin", "python")); err != nil {
		t.Fatalf("the converge removed the venv an engine may be running from: %v", err)
	}
	// Still in use: kept. No longer in use: reclaimed.
	if removed, _ := inst.PruneUnused(map[string]bool{"0.20.0": true}); len(removed) != 0 {
		t.Errorf("reclaimed %v while it was in use", removed)
	}
	if removed, err := inst.PruneUnused(nil); err != nil || len(removed) != 1 || removed[0] != "0.20.0" {
		t.Errorf("PruneUnused(nothing in use) = %v, %v; want [0.20.0]", removed, err)
	}
	// And the host is now settled: a second pass does nothing.
	again, err := ConvergeVLLM(context.Background(), realConvergeDeps(inst))
	if err != nil {
		t.Fatalf("second ConvergeVLLM: %v", err)
	}
	if again.Install {
		t.Errorf("second pass decided to install (reason: %s)", again.Reason)
	}
}

// A companion-pin move — the vLLM version is the pin, the transformers
// constraint recorded beside it is not — used to pip-install into the
// live venv. It now builds beside it (waired-agent#1431).
func TestConvergeVLLM_CompanionPinMoveBuildsBesideTheLiveVenv(t *testing.T) {
	dir := t.TempDir()
	inst := newRecordingInstaller(t, dir)
	if _, err := inst.Install(context.Background(), InstallOpts{Version: VLLMPinnedVersion}, nil); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	old := WantedVLLMPins()
	old.Transformers = "transformers<5.0"
	if err := writeVLLMPins(filepath.Join(dir, VLLMPinnedVersion), old); err != nil {
		t.Fatal(err)
	}
	r := inst.Runner.(*scriptedRunner)
	r.calls = nil

	decision, err := ConvergeVLLM(context.Background(), realConvergeDeps(inst))
	if err != nil || !decision.Install {
		t.Fatalf("ConvergeVLLM = %+v, %v; want a rebuild for the transformers move", decision, err)
	}
	liveVenv := filepath.Join(dir, VLLMPinnedVersion, ".venv")
	for _, c := range r.calls {
		if joined := c.binary + " " + strings.Join(c.args, " "); strings.Contains(joined, liveVenv) {
			t.Errorf("the companion-pin move ran against the live venv: %s", joined)
		}
	}
	active, ok := inst.Active()
	if !ok || active.Dir != VLLMPinnedVersion+"~2" {
		t.Errorf("Active() = %+v, %v; want %s~2", active, ok, VLLMPinnedVersion)
	}
	if pins, _ := inst.ActivePins(); pins.Transformers != WantedVLLMPins().Transformers {
		t.Errorf("active pins = %+v, want this build's set", pins)
	}
}

// The lock excludes a second holder across open files, as two processes
// are excluded, and tells a waiter it is waiting (waired-agent#1431).
func TestVLLMInstallerLock_ExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	a, b := newRecordingInstaller(t, dir), newRecordingInstaller(t, dir)
	unlock, err := a.Lock(context.Background(), nil)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	waited := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*vllmLockPoll)
	defer cancel()
	if _, err := b.Lock(ctx, func() { waited++ }); err == nil {
		t.Fatal("a second holder got the lock while the first held it")
	}
	if waited != 1 {
		t.Errorf("onWait called %d times, want once", waited)
	}
	unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	unlock2, err := b.Lock(ctx2, nil)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	unlock2()
}

// RemoveUVIfNoVenvs takes the managed uv, its cache and the Python uv
// installed away only when no venv is left for them to build, reconcile
// or run (waired-ai/waired#1435).
func TestVLLMInstaller_RemoveUVIfNoVenvs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dirs     []string // under BaseDir
		wantGone bool
	}{
		{"no venvs", nil, true},
		{"only a failed build kept for inspection", []string{"0.29.0.failed-20260916/.venv"}, true},
		{"a venv remains", []string{"0.28.0/.venv"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			base := filepath.Join(state, "runtimes", "vllm")
			for _, d := range append([]string{""}, tc.dirs...) {
				if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			inst := NewVLLMInstallerAt(base)
			if err := os.MkdirAll(filepath.Join(inst.UV.CacheDir(), "wheels-v5"), 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteExec(t, uvStubPath(t, inst.UV.Root), "#!/bin/sh\n")
			python := filepath.Join(base, "python", "cpython-3.12-linux-x86_64-gnu", "bin")
			if err := os.MkdirAll(python, 0o755); err != nil {
				t.Fatal(err)
			}

			removed, err := inst.RemoveUVIfNoVenvs()
			if err != nil {
				t.Fatalf("RemoveUVIfNoVenvs: %v", err)
			}
			for _, dir := range []string{inst.UV.Root, filepath.Join(base, "python")} {
				_, statErr := os.Stat(dir)
				if gone := os.IsNotExist(statErr); gone != tc.wantGone || removed != tc.wantGone {
					t.Errorf("%s: removed=%v gone=%v, want %v", dir, removed, gone, tc.wantGone)
				}
			}
		})
	}

	t.Run("nothing to remove", func(t *testing.T) {
		inst := NewVLLMInstallerAt(filepath.Join(t.TempDir(), "runtimes", "vllm"))
		if removed, err := inst.RemoveUVIfNoVenvs(); removed || err != nil {
			t.Errorf("removed=%v err=%v, want false, nil", removed, err)
		}
	})
}
