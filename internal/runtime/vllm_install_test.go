//go:build linux

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptedRunner is a fake InstallRunner that scripts each subprocess
// call by binary name and remembers the order of calls so tests can
// assert on the install pipeline shape.
//
// When asked to run `uv venv ... <dir>` it also materialises a fake
// venv layout (bin/python touch-file) at <dir> so the installer's
// downstream stages and Active()'s symlink stat have a real file to
// look at. This avoids each test having to manually scaffold the
// venv shape.
type scriptedRunner struct {
	t       *testing.T
	calls   []scriptedCall
	respond func(call scriptedCall) (lines []string, err error)
}

type scriptedCall struct {
	binary string
	args   []string
}

func (r *scriptedRunner) Run(_ context.Context, binary string, args, env []string, onLine func(string)) error {
	c := scriptedCall{binary: binary, args: append([]string(nil), args...)}
	_ = env
	r.calls = append(r.calls, c)
	if len(args) >= 2 && args[0] == "venv" {
		// `uv venv ... --python <py> <dir>` (last arg is the venv dir
		// in our installer's invocation).
		venvDir := args[len(args)-1]
		_ = os.MkdirAll(filepath.Join(venvDir, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(venvDir, "bin", "python"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	lines, err := r.respond(c)
	for _, l := range lines {
		onLine(l)
	}
	return err
}

func TestVLLMInstall_HappyPath(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	uvBin := filepath.Join(uvDir, "uv")
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\necho 0.11.8\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &scriptedRunner{t: t, respond: func(scriptedCall) ([]string, error) {
		return []string{"ok"}, nil
	}}
	inst := &VLLMInstaller{
		BaseDir: dir,
		UV:      &UVResolver{BinDir: uvDir},
		Runner:  r,
		Now:     fakeNow,
	}

	progress := []InstallProgress{}
	res, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, func(p InstallProgress) {
		progress = append(progress, p)
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if res.Version != "0.11.0" {
		t.Errorf("Version = %q", res.Version)
	}
	if !strings.HasSuffix(res.VenvPath, "/0.11.0/.venv") {
		t.Errorf("VenvPath = %q, expected suffix /0.11.0/.venv", res.VenvPath)
	}
	if !strings.HasSuffix(res.BinDir, "/0.11.0/.venv/bin") {
		t.Errorf("BinDir = %q", res.BinDir)
	}

	// Three subprocess invocations: uv venv, uv pip install, python verify.
	if len(r.calls) != 3 {
		t.Fatalf("calls = %d, want 3 (venv, pip install, verify)", len(r.calls))
	}
	// First two calls go through whatever uv UVResolver picked (system
	// uv or the test's stub) — assert by command shape, not path,
	// since the system PATH may shadow the stub.
	_ = uvBin
	if filepath.Base(r.calls[0].binary) != "uv" || r.calls[0].args[0] != "venv" || r.calls[0].args[1] != "--python" {
		t.Errorf("first call should be `uv venv --python ...`, got %s %v", r.calls[0].binary, r.calls[0].args)
	}
	if filepath.Base(r.calls[1].binary) != "uv" || r.calls[1].args[0] != "pip" || r.calls[1].args[1] != "install" {
		t.Errorf("second call should be `uv pip install ...`, got %s %v", r.calls[1].binary, r.calls[1].args)
	}
	wantPipPackages := []string{"vllm==0.11.0", "hf_transfer==" + HFTransferPinnedVersion}
	for _, pkg := range wantPipPackages {
		if !sliceContains(r.calls[1].args, pkg) {
			t.Errorf("pip install missing %q, got %v", pkg, r.calls[1].args)
		}
	}
	if !strings.HasSuffix(r.calls[2].binary, "/0.11.0/.venv/bin/python") {
		t.Errorf("third call should be venv python, got %s", r.calls[2].binary)
	}
	if r.calls[2].args[0] != "-c" || !strings.Contains(r.calls[2].args[1], "torch.cuda") {
		t.Errorf("third call should be the verify snippet, got %v", r.calls[2].args)
	}

	// Progress: at least one event per stage, in order.
	stagesSeen := []InstallStage{}
	for _, p := range progress {
		if len(stagesSeen) == 0 || stagesSeen[len(stagesSeen)-1] != p.Stage {
			stagesSeen = append(stagesSeen, p.Stage)
		}
	}
	wantStages := []InstallStage{StageResolveUV, StageCreateVenv, StagePipInstall, StageVerify, StageActivate}
	for i, want := range wantStages {
		if i >= len(stagesSeen) || stagesSeen[i] != want {
			t.Errorf("progress stages[%d] = %v, want %v (got %v)", i, stagesSeen, wantStages, stagesSeen)
			break
		}
	}

	// Active() reads the symlink the activate stage just wrote.
	active, ok := inst.Active()
	if !ok {
		t.Fatalf("Active() reported no install after Install succeeded")
	}
	if active.Version != "0.11.0" {
		t.Errorf("Active.Version = %q", active.Version)
	}
}

func TestVLLMInstall_PipFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uvDir, "uv"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &scriptedRunner{respond: func(c scriptedCall) ([]string, error) {
		if len(c.args) > 0 && c.args[0] == "pip" {
			return []string{"ERROR: Could not find a version that satisfies the requirement vllm==0.11.0"}, errors.New("exit status 1")
		}
		return nil, nil
	}}
	inst := &VLLMInstaller{
		BaseDir: dir, UV: &UVResolver{BinDir: uvDir}, Runner: r, Now: fakeNow,
	}
	_, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil)
	if err == nil {
		t.Fatalf("expected install to fail when pip exits non-zero")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "0.11.0")); !os.IsNotExist(statErr) {
		t.Errorf("expected version dir to be rolled back, got stat err = %v", statErr)
	}
}

func TestVLLMInstall_KeepFailedRetainsBrokenVenv(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uvDir, "uv"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scriptedRunner{respond: func(c scriptedCall) ([]string, error) {
		if len(c.args) > 0 && c.args[0] == "pip" {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}}
	inst := &VLLMInstaller{
		BaseDir: dir, UV: &UVResolver{BinDir: uvDir}, Runner: r, Now: fakeNow,
	}
	_, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0", KeepFailed: true}, nil)
	if err == nil {
		t.Fatalf("expected install to fail")
	}
	entries, _ := os.ReadDir(dir)
	foundFailedSuffix := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0.11.0.failed-") {
			foundFailedSuffix = true
			break
		}
	}
	if !foundFailedSuffix {
		t.Errorf("expected a 0.11.0.failed-* dir for inspection, got %v", dirNames(entries))
	}
}

func TestVLLMInstall_VerifyFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uvDir, "uv"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scriptedRunner{respond: func(c scriptedCall) ([]string, error) {
		// Only the verify (python -c ...) step fails.
		if len(c.args) >= 2 && c.args[0] == "-c" && strings.Contains(c.args[1], "torch.cuda") {
			return []string{"compute capability 7.5 below the SM_80 floor"}, errors.New("exit status 1")
		}
		return nil, nil
	}}
	inst := &VLLMInstaller{
		BaseDir: dir, UV: &UVResolver{BinDir: uvDir}, Runner: r, Now: fakeNow,
	}
	_, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil)
	if err == nil {
		t.Fatalf("expected verify failure to surface")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "0.11.0")); !os.IsNotExist(statErr) {
		t.Errorf("expected verify failure to roll back venv, got stat err = %v", statErr)
	}
}

func TestVLLMInstall_ActiveBeforeInstallReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	inst := &VLLMInstaller{BaseDir: dir, UV: NewUVResolver(), Runner: &scriptedRunner{}, Now: fakeNow}
	if _, ok := inst.Active(); ok {
		t.Errorf("Active() should be false before any install")
	}
}

func TestVLLMInstall_Uninstall(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uvDir, "uv"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scriptedRunner{respond: func(scriptedCall) ([]string, error) { return nil, nil }}
	inst := &VLLMInstaller{
		BaseDir: dir, UV: &UVResolver{BinDir: uvDir}, Runner: r, Now: fakeNow,
	}
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := inst.Active(); !ok {
		t.Fatalf("expected Active=true after Install")
	}
	if err := inst.Uninstall(context.Background(), "0.11.0"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, ok := inst.Active(); ok {
		t.Errorf("Uninstall did not drop the current symlink")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "0.11.0")); !os.IsNotExist(statErr) {
		t.Errorf("Uninstall did not remove the version dir")
	}
}

func TestExtractInstallPercent(t *testing.T) {
	cases := map[string]int{
		"Downloading torch (700 MB) 47%": 47,
		"Resolving deps... 12%":          12,
		"download progress 99.9%":        -1, // fractional rejected
		"no percent here":                -1,
		"100%":                           100,
		"  trimmed 5%":                   5,
		"more than three digits 1234%":   -1, // 4-digit run rejected
	}
	for in, want := range cases {
		if got := extractInstallPercent(in); got != want {
			t.Errorf("extractInstallPercent(%q) = %d, want %d", in, got, want)
		}
	}
}

func dirNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// fakeNow returns a fixed timestamp so install records are
// reproducible across test runs.
func fakeNow() time.Time {
	return time.Date(2026, 5, 3, 4, 30, 0, 0, time.UTC)
}

// TestNewVLLMInstallerAt_UsesGivenBaseDir guards the #525 fix: the
// explicit constructor must root the installer at the caller-supplied
// path (which the CLI / daemon pass as <state-dir>/runtimes/vllm) so a
// sudo-run install and the User=waired daemon agree on one location
// regardless of $HOME.
func TestNewVLLMInstallerAt_UsesGivenBaseDir(t *testing.T) {
	want := filepath.Join(t.TempDir(), "runtimes", "vllm")
	if got := NewVLLMInstallerAt(want).BaseDir; got != want {
		t.Errorf("NewVLLMInstallerAt(%q).BaseDir = %q, want %q", want, got, want)
	}
}

// TestNewVLLMInstaller_LegacyDefault confirms the back-compat
// constructor still resolves the $HOME-relative default (used by the GPU
// e2e helper) after the delegation refactor.
func TestNewVLLMInstaller_LegacyDefault(t *testing.T) {
	if got := NewVLLMInstaller().BaseDir; got != defaultVLLMBaseDir() {
		t.Errorf("NewVLLMInstaller().BaseDir = %q, want defaultVLLMBaseDir() %q", got, defaultVLLMBaseDir())
	}
}
