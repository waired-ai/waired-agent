//go:build linux

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	// env is RECORDED, not dropped. It used to be `_ = env`, which made
	// waired-agent#778 unwritable as a test: the defect was an environment
	// variable the installer did not set, and a fake that discards the
	// parameter cannot fail on it (CLAUDE.md §Test discipline — "a fake that
	// drops a parameter is a defect: it makes the failing case unwritable").
	env []string
}

func (r *scriptedRunner) Run(_ context.Context, binary string, args, env []string, onLine func(string)) error {
	c := scriptedCall{
		binary: binary,
		args:   append([]string(nil), args...),
		env:    append([]string(nil), env...),
	}
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

// The venv's interpreter must live inside BaseDir, not in the installing
// user's home.
//
// PRODUCT CONTRACT — waired-agent#778. `uv venv --python <ver>` downloads a
// managed interpreter into $UV_PYTHON_INSTALL_DIR (default
// ~/.local/share/uv/python) and SYMLINKS the venv's bin/python at it. The
// installer runs elevated, so on Linux that default is /root/.local/share,
// and /root is 0700: the unprivileged daemon user cannot follow the symlink.
// Active() then fails its os.Stat and answers "no install" on a host whose
// venv is complete — which is the whole of #778. Reproduced on real hardware
// 2026-08-14 (evidence: verify-20260815-l56/sv-mag/M3repro/00-FINDING.md).
//
// Pointing UV_PYTHON_INSTALL_DIR inside BaseDir puts the interpreter under
// the state dir, where the executor's ownership hand-off
// (service.FixStateOwnership -> chownRecursive) already reaches it.
func TestVLLMInstall_PutsThePythonInstallDirUnderBaseDir(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	uvBin := filepath.Join(uvDir, "uv")
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\necho 0.11.8\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scriptedRunner{t: t, respond: func(scriptedCall) ([]string, error) {
		return []string{"ok"}, nil
	}}
	inst := &VLLMInstaller{BaseDir: dir, UV: &UVResolver{BinDir: uvDir}, Runner: r, Now: fakeNow}

	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Every stage that can materialise or resolve an interpreter has to
	// carry these: the venv creation downloads it, and pip/verify resolve
	// through the same interpreter. One stage missing it is one stage that
	// can still reach into the home directory.
	//
	// UV_NO_CONFIG is here for the other half of that sentence (#843). uv
	// finds uv.toml / pyproject.toml by walking UP from the current
	// directory, which this installer inherits from whoever invoked it —
	// observed on a real host as `failed to open file
	// /home/<someone>/uv.toml: Permission denied` when the install ran as
	// the service user from a login shell's home.
	for _, want := range []string{
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(dir, "python"),
		"UV_NO_CONFIG=1",
	} {
		for _, c := range r.calls {
			if !slices.Contains(c.env, want) {
				t.Errorf("call %s %v ran without %s (env=%v)", c.binary, c.args, want, c.env)
			}
		}
	}
	if len(r.calls) == 0 {
		t.Fatal("no subprocess calls recorded")
	}
}

// Active() must not report "no install" for a venv it simply cannot read.
//
// PRODUCT CONTRACT — waired-agent#778. "absent" and "present but not
// readable by this user" are different answers, and only the first means
// "install it". Collapsing them is what made the rc9 host wait forever: the
// engine decision said "no engine viable", the setup projection said
// engine_installed=false, and init's arrival predicate therefore never armed
// its grace — all from one os.Stat that failed with EACCES rather than
// ENOENT. Same shape as #67, where a missing probe answered "no GPU"
// instead of erroring.
func TestVLLMActive_UnreadableVenvIsNotReportedAsAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny root, so this case is unreachable")
	}
	base := t.TempDir()
	venvBin := filepath.Join(base, "0.11.0", ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venvBin, "python"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("0.11.0", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	// Deny traversal of the bin dir: the interpreter is there, we just
	// cannot reach it — the shape a /root-owned symlink target produces.
	if err := os.Chmod(venvBin, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(venvBin, 0o755) })

	inst := NewVLLMInstallerAt(base)
	res, err := inst.ActiveErr()
	if err == nil {
		t.Fatalf("ActiveErr returned no error for an unreadable venv (res=%+v)", res)
	}
	if errors.Is(err, ErrVLLMNotInstalled) {
		t.Errorf("an unreadable venv was reported as not installed: %v", err)
	}
	if !strings.Contains(err.Error(), "python") {
		t.Errorf("error %q does not name what could not be read", err)
	}
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
	// huggingface_hub is asserted here because it was NOT, for as long as
	// the request said `huggingface_hub[cli]` — an extra huggingface_hub
	// 1.x had removed, which uv warned about and resolved past. Nothing
	// in this file would have noticed (waired-agent#263).
	wantPipPackages := []string{
		"vllm==0.11.0",
		"hf_transfer==" + HFTransferPinnedVersion,
		"huggingface_hub>=1.0",
		"ninja",
	}
	for _, pkg := range wantPipPackages {
		if !sliceContains(r.calls[1].args, pkg) {
			t.Errorf("pip install missing %q, got %v", pkg, r.calls[1].args)
		}
	}
	// The extra is gone and must stay gone: asking for it again resolves
	// to plain huggingface_hub with a warning, which is the "guarantee"
	// that turned out to be nothing.
	for _, arg := range r.calls[1].args {
		if strings.Contains(arg, "huggingface_hub[") || strings.Contains(arg, "huggingface-hub[") {
			t.Errorf("pip install asks for a huggingface_hub extra (%q); 1.x removed `cli` "+
				"and ships the console scripts in the base package", arg)
		}
	}
	if !strings.HasSuffix(r.calls[2].binary, "/0.11.0/.venv/bin/python") {
		t.Errorf("third call should be venv python, got %s", r.calls[2].binary)
	}
	if r.calls[2].args[0] != "-c" || !strings.Contains(r.calls[2].args[1], "torch.cuda") {
		t.Errorf("third call should be the verify snippet, got %v", r.calls[2].args)
	}
	// The venv the pipeline hands over must be able to fetch weights, and
	// the pip request alone cannot promise that (waired-agent#263). This
	// asserts the snippet actually passed to the venv python looks for the
	// same two console-script names resolveVenvHFCLI will later resolve.
	//
	// A record of today's snippet rather than a behavioural contract: the
	// real check runs inside python on a CUDA host, which no unit test
	// here has. What it does buy is that deleting the check fails a test
	// instead of going unnoticed until the first safetensors pull.
	for _, name := range []string{"'hf'", "'huggingface-cli'"} {
		if !strings.Contains(r.calls[2].args[1], name) {
			t.Errorf("verify snippet does not look for %s; a venv with no downloader "+
				"would pass verify and fail at the first model pull", name)
		}
	}

	// Progress: at least one event per stage, in order.
	stagesSeen := []InstallStage{}
	for _, p := range progress {
		if len(stagesSeen) == 0 || stagesSeen[len(stagesSeen)-1] != p.Stage {
			stagesSeen = append(stagesSeen, p.Stage)
		}
	}
	wantStages := []InstallStage{StageResolveUV, StageCreateVenv, StagePipInstall, StageToolchain, StageVerify, StageActivate}
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

// TestExtractInstallPercent is a RECORD of today's behaviour, not a
// product contract. The inputs below were invented, and waired-agent#255
// established that uv emits no percentage at all when its output is
// piped — which is every path the installer takes. The matcher is kept
// because it still fires for any tool line that does carry an NN%, but
// nothing user-facing depends on it any more: the pip-install stage is
// byte-denominated now (uv_progress.go).
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

// TestVLLMInstall_PipStageCarriesBytes pins the whole of
// waired-agent#255's producer half: the byte figures ride the
// pip-install stage and only that stage, and they come from uv's real
// announcements rather than an estimate.
func TestVLLMInstall_PipStageCarriesBytes(t *testing.T) {
	dir := t.TempDir()
	uvDir := t.TempDir()
	uvBin := filepath.Join(uvDir, "uv")
	if err := os.WriteFile(uvBin, []byte("#!/bin/sh\necho 0.11.8\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Verbatim uv 0.11.26 output for the pip call; anything else answers
	// with the non-transfer chatter the other stages produce.
	r := &scriptedRunner{t: t, respond: func(c scriptedCall) ([]string, error) {
		if len(c.args) >= 2 && c.args[0] == "pip" && c.args[1] == "install" {
			return []string{
				"Resolved 190 packages in 2.47s",
				"Downloading torch (506.1MiB)",
				"Downloading nvidia-nvjitlink (38.8MiB)",
				" Downloaded nvidia-nvjitlink",
				" Downloaded torch",
				"Prepared 190 packages in 1m 20s",
				"Installed 190 packages in 53ms",
			}, nil
		}
		return []string{"ok"}, nil
	}}
	inst := &VLLMInstaller{BaseDir: dir, UV: &UVResolver{BinDir: uvDir}, Runner: r, Now: fakeNow}

	var progress []InstallProgress
	if _, err := inst.Install(context.Background(), InstallOpts{Version: "0.11.0"}, func(p InstallProgress) {
		progress = append(progress, p)
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	wantTotal := mib(506.1) + mib(38.8)
	var sawTotal, maxCompleted int64
	for _, p := range progress {
		if p.Stage != StagePipInstall {
			if p.CompletedBytes != 0 || p.TotalBytes != 0 || p.BytesPerSec != 0 {
				t.Errorf("stage %s carries bytes %d/%d/%d — only pip-install transfers anything",
					p.Stage, p.CompletedBytes, p.TotalBytes, p.BytesPerSec)
			}
			continue
		}
		if p.TotalBytes > sawTotal {
			sawTotal = p.TotalBytes
		}
		if p.CompletedBytes > maxCompleted {
			maxCompleted = p.CompletedBytes
		}
		if p.CompletedBytes > p.TotalBytes {
			t.Errorf("completed %d > total %d — the two must stay in the same units",
				p.CompletedBytes, p.TotalBytes)
		}
	}
	if sawTotal != wantTotal {
		t.Errorf("pip-install total = %d, want %d (the two announced sizes)", sawTotal, wantTotal)
	}
	if maxCompleted != wantTotal {
		t.Errorf("pip-install completed peaked at %d, want %d — the row must finish full",
			maxCompleted, wantTotal)
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
