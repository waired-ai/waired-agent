//go:build linux

// vLLM installer: Linux-only. See vllm.go for the cross-platform
// rationale (Windows / macOS use the stub files).

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The pins this installer builds from — VLLMPinnedVersion,
// HFTransferPinnedVersion, TransformersConstraint, VLLMPythonVersion —
// live in vllm_pins.go, untagged, because the converge compares the
// whole set on every platform (#843).

// VLLMVerifyImports is the python snippet the install pipeline runs
// to confirm the venv is truly usable: vllm importable, torch sees
// CUDA, GPU compute capability ≥ 10 (= Blackwell-ready), and a
// huggingface_hub console script present to download weights with.
//
// The downloader check is here rather than left to the pip request
// because the request cannot enforce it. huggingface_hub 1.x removed
// the `cli` extra, so `huggingface_hub[cli]` resolved to plain
// huggingface_hub with only a warning — the guarantee the extra was
// written to provide had quietly become the transitive accident it was
// meant to replace (waired-agent#263). Asserting the binary after the
// install is the form that cannot go stale: it holds whether the extra
// exists, whether it comes back, and whether the scripts move again.
//
// It looks for the same two names in the same order as
// resolveVenvHFCLI (cmd/waired-agent/inference_vllm_linux.go), beside
// this interpreter, so what verify accepts is exactly what the daemon
// will later resolve. Without it a venv missing the downloader passes
// verify clean and fails at the first safetensors pull instead.
const VLLMVerifyImports = `
import os, sys, vllm, torch
assert torch.cuda.is_available(), 'torch.cuda.is_available() is False'
cap = torch.cuda.get_device_capability(0)
if cap[0] < 8:
    sys.exit(f'compute capability {cap[0]}.{cap[1]} below the SM_80 floor')
bindir = os.path.dirname(sys.executable)
if not any(os.path.exists(os.path.join(bindir, n)) for n in ('hf', 'huggingface-cli')):
    sys.exit(f'no huggingface_hub console script (hf, huggingface-cli) in {bindir}: '
             'the venv cannot download model weights')
print(vllm.__version__)
`

// InstallStage identifies which step of the install pipeline a
// progress event belongs to. The CLI uses this to render its
// "[stage/total]" indicator.
type InstallStage string

const (
	StageResolveUV  InstallStage = "resolve-uv"
	StageCreateVenv InstallStage = "create-venv"
	StagePipInstall InstallStage = "pip-install"
	StageVerify     InstallStage = "verify"
	StageActivate   InstallStage = "activate"
)

// InstallProgress is one update emitted while VLLMInstaller.Install
// runs. The CLI converts these to the "[3/5] Installing vllm... 47%"
// presentation described in the plan.
type InstallProgress struct {
	Stage InstallStage
	Step  int
	// Total is the number of STAGES (5), not a byte count — it predates
	// the byte fields below and keeps its meaning.
	Total   int
	Percent int // 0-100 when the underlying tool reports it; -1 otherwise
	Message string

	// CompletedBytes / TotalBytes / BytesPerSec are the transfer figures
	// for the one byte-denominated stage, pip-install. They are read off
	// uv's own per-package announcements (uv_progress.go) rather than
	// estimated, and are 0 on every other stage.
	//
	// Named for the wire they end up on (SetupStep / SetupExecutorRequest)
	// so the setup sink is a straight field copy, and named apart from
	// Total above so the stage counter cannot be mistaken for a size
	// (waired-agent#255).
	//
	// 0 = unknown, for the rate as much as for the counts. A stall is
	// derived from CompletedBytes not advancing, matching #197's rule.
	CompletedBytes int64
	TotalBytes     int64
	BytesPerSec    int64
}

// InstallResult is the venv that Install successfully materialised.
type InstallResult struct {
	Version     string
	VenvPath    string
	BinDir      string
	InstalledAt time.Time
}

// InstallOpts customises what gets installed. Defaults are the pin set
// in vllm_pins.go: VLLMPinnedVersion / HFTransferPinnedVersion /
// VLLMPythonVersion.
type InstallOpts struct {
	Version           string
	HFTransferVersion string
	PythonVersion     string // e.g. "3.12"
	KeepFailed        bool   // leave the broken venv in place under ".failed-<ts>"
	ExtraPipPackages  []string

	// Recreate replaces an environment that is already there instead of
	// reconciling the wheels into it. It is the difference between the
	// two verbs: `waired runtimes install vllm` answers "put a clean
	// environment here" and sets it; the converge answers "make what is
	// here match" and does not, because it may be running while the host
	// serves (#843). It is also the only way to move the INTERPRETER — a
	// reconcile keeps the one the venv was built with.
	Recreate bool
}

// InstallRunner is the test seam for the uv / python subprocesses
// the installer spawns. Mirrors HFRunner's shape so the tests look
// the same.
type InstallRunner interface {
	Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error
}

// VLLMInstaller orchestrates the venv lifecycle: uv venv build →
// pip install vllm + hf_transfer → torch/vllm verification →
// `current` symlink swap. Stateless across Install calls.
type VLLMInstaller struct {
	BaseDir string        // typically <XDG_DATA_HOME>/waired/runtimes/vllm
	UV      *UVResolver   // for resolving a uv binary
	Runner  InstallRunner // for the venv / pip / python subprocesses
	Now     func() time.Time
}

// NewVLLMInstallerAt wires the installer rooted at an explicit baseDir.
// Callers that run under sudo (`waired runtimes install vllm`) and the
// daemon (which resolves the venv to decide engine viability) must pass
// the *same* `<state-dir>/runtimes/vllm` path — a $HOME-relative default
// diverges between root (HOME=/root) and the User=waired daemon
// (HOME=/var/lib/waired), so the daemon never finds a sudo-run install
// (#525). The runner is the real subprocess spawner; tests inject a fake.
func NewVLLMInstallerAt(baseDir string) *VLLMInstaller {
	return &VLLMInstaller{
		BaseDir: baseDir,
		UV:      NewUVResolver(),
		Runner:  DefaultInstallRunner{},
		Now:     time.Now,
	}
}

// NewVLLMInstaller wires the installer with the legacy $HOME-relative
// default base dir ($XDG_DATA_HOME/waired/runtimes/vllm). Prefer
// NewVLLMInstallerAt with an explicit `<state-dir>/runtimes/vllm` so the
// installer and daemon agree on one path regardless of who ($HOME) runs
// it (#525); this constructor is retained for the GPU e2e helper.
func NewVLLMInstaller() *VLLMInstaller {
	return NewVLLMInstallerAt(defaultVLLMBaseDir())
}

// Install builds the venv for opts.Version, or reconciles the wheels
// into one that is already there — opts.Recreate decides which, and
// stage 2 below records what uv actually does with an existing
// environment. On failure a venv THIS CALL created is removed (or
// relocated to .failed-<ts> when KeepFailed is set) so the next attempt
// starts clean; one that was already here is left alone (#843).
//
// The five-stage pipeline maps to plan §3.6:
//
//  1. Resolve uv (no-op when uv was already on PATH or cached).
//  2. Create the versioned venv via `uv venv --python <py> <dir>/.venv`.
//  3. Install vllm + hf_transfer (+ extras) via `uv pip install`.
//  4. Verify the install runs `python -c "import vllm, torch; ..."`.
//  5. Activate by atomically swapping the `current` symlink.
func (i *VLLMInstaller) Install(ctx context.Context, opts InstallOpts, onProgress func(InstallProgress)) (InstallResult, error) {
	if onProgress == nil {
		onProgress = func(InstallProgress) {}
	}
	version := opts.Version
	if version == "" {
		version = VLLMPinnedVersion
	}
	hf := opts.HFTransferVersion
	if hf == "" {
		hf = HFTransferPinnedVersion
	}
	py := opts.PythonVersion
	if py == "" {
		py = VLLMPythonVersion
	}
	const totalStages = 5

	// Keep uv's managed interpreter inside BaseDir (waired-agent#778).
	//
	// `uv venv --python <ver>` materialises an interpreter under
	// $UV_PYTHON_INSTALL_DIR — default ~/.local/share/uv/python — and points
	// the venv's bin/python at it by SYMLINK. This installer runs elevated,
	// so on Linux that default resolves inside /root, which is 0700: the
	// unprivileged daemon user cannot follow the symlink, Active()'s
	// os.Stat fails, and a complete venv reads as "no install" for the life
	// of the host. Under BaseDir the interpreter is inside the state dir the
	// executor hands to the service user (service.FixStateOwnership), so the
	// same chown that covers the venv covers what it points at.
	//
	// Passed to every stage that can materialise or resolve an interpreter,
	// not just the venv creation: one stage without it is one stage that can
	// still reach into the home directory.
	//
	// UV_NO_CONFIG for a second reason, found on the same host (#843): uv
	// discovers uv.toml / pyproject.toml by walking UP from the current
	// directory, and this installer inherits whatever directory its
	// caller was standing in. `sudo waired runtimes install vllm` run
	// from a home directory the target user cannot read fails outright —
	//
	//   error: failed to open file `/home/<someone>/uv.toml`: Permission denied
	//
	// — and run from a directory that HAS a uv.toml silently resolves
	// against settings nobody meant to apply to the engine. The venv this
	// product builds is defined by the arguments above and nothing else.
	uvEnv := []string{
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(i.BaseDir, "python"),
		"UV_NO_CONFIG=1",
	}

	// Stage 1: resolve uv.
	onProgress(InstallProgress{Stage: StageResolveUV, Step: 1, Total: totalStages, Percent: -1, Message: "resolving uv binary..."})
	uvBin, err := i.UV.Resolve(ctx, "")
	if err != nil {
		return InstallResult{}, fmt.Errorf("vllm install: %w", err)
	}

	versionDir := filepath.Join(i.BaseDir, version)
	venvDir := filepath.Join(versionDir, ".venv")
	// Whether this call is the one that brings the version directory into
	// existence decides what a failure is allowed to delete (#843).
	_, statErr := os.Stat(versionDir)
	ours := os.IsNotExist(statErr)
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("vllm install: mkdir version dir: %w", err)
	}

	// Stage 2: create venv.
	//
	// uv REFUSES to create over an existing environment — "A virtual
	// environment already exists at ...", exit 2, with a hint to pass
	// --clear. The comment that stood here said the opposite ("uv exits
	// successfully without rebuilding, which gives us idempotency for
	// free"); against the uv this product resolves it does not, so every
	// re-entry into an existing version directory failed at this line and
	// then had the directory removed under it. Found on a real host
	// (#843); the fake runner in the tests had made `uv venv` succeed on
	// an existing directory, so nothing here could see it.
	//
	// So the caller's intent has to be stated rather than assumed:
	//
	//   - Recreate (the explicit `waired runtimes install vllm`) means
	//     "put a clean environment here" — pass --clear.
	//   - Otherwise (the converge) means "make what is here match" — keep
	//     the environment and let the pip stage below re-resolve the
	//     wheels into it. That is what makes a companion-pin move cost a
	//     small wheel instead of 4 GB, and it is why a converge can never
	//     remove the environment the host may be serving from.
	//
	// Reconciling in place cannot change the INTERPRETER, so the converge
	// must not be handed that job: DecideVLLMConverge reports an
	// interpreter-pin move as blocked and names the reinstall. Without
	// that, this would pip-install forever and then record a pin set the
	// venv does not have.
	if usable := venvInterpreter(venvDir); usable && !opts.Recreate {
		onProgress(InstallProgress{Stage: StageCreateVenv, Step: 2, Total: totalStages, Percent: -1,
			Message: "using the virtual environment already here (Python " + py + ")..."})
	} else {
		onProgress(InstallProgress{Stage: StageCreateVenv, Step: 2, Total: totalStages, Percent: -1, Message: "creating venv (Python " + py + ")..."})
		venvArgs := []string{"venv", "--python", py, venvDir}
		if _, err := os.Stat(venvDir); err == nil {
			// Present but not usable, or a deliberate rebuild. Either
			// way there is nothing here worth keeping.
			venvArgs = []string{"venv", "--clear", "--python", py, venvDir}
		}
		if err := i.runCapturing(ctx, uvBin, venvArgs, uvEnv, onProgress, StageCreateVenv, 2, totalStages, nil); err != nil {
			i.maybeRollback(versionDir, opts.KeepFailed, ours)
			return InstallResult{}, fmt.Errorf("vllm install: uv venv: %w", err)
		}
	}

	// Stage 3: pip install. Pass --python so uv doesn't infer from PATH.
	//
	// This stage is ~90% of the wall clock and all of the transfer, so it
	// is the one that gets byte-denominated progress — everything the
	// browser wizard's engine_download row is drawn from comes from here
	// (waired-agent#255).
	pipBytes := newUVDownloadTracker()
	onProgress(InstallProgress{Stage: StagePipInstall, Step: 3, Total: totalStages, Percent: -1, Message: "installing vllm==" + version + " hf_transfer==" + hf + " (this may take 5-15 minutes, ~4 GB download)..."})
	pipArgs := []string{
		"pip", "install",
		"--python", filepath.Join(venvDir, "bin", "python"),
		"vllm==" + version,
		"hf_transfer==" + hf,
		// huggingface_hub ships the `hf` / `huggingface-cli` binary the
		// agent's HFPuller (internal/download/hf.go, ResolveHFCLI) shells
		// out to for the safetensors download. vLLM already pulls
		// huggingface_hub in transitively; naming it explicitly keeps it
		// from being only a transitive accident.
		//
		// The floor, not the `[cli]` extra that used to be here. 1.x
		// REMOVED that extra and ships the console scripts in the base
		// package instead, so `huggingface_hub[cli]` resolved to plain
		// huggingface_hub with a warning uv prints and carries on past:
		//
		//   warning: The package `huggingface-hub==1.25.1` does not have
		//   an extra named `cli`
		//
		// The venv kept working, but for exactly the reason the extra was
		// written to stop relying on. `>=1.0` states the real
		// requirement — the version from which the scripts are in the
		// base package — and leaves the upper end to uv so it still
		// resolves the one vllm pins (waired-agent#263).
		//
		// Stating it is not the same as guaranteeing it: VLLMVerifyImports
		// asserts the binary exists after the install, which is what makes
		// the guarantee real rather than declared.
		"huggingface_hub>=1.0",
		// vllm 0.24's flashinfer JIT-compiles CUDA ops at engine
		// start-up and shells out to `ninja`. The wheel arrives
		// transitively today, but name it explicitly so a resolver
		// change cannot leave the venv unable to serve. (VLLMAdapter
		// puts the venv bin dir on the child PATH so this binary is
		// found.) No post-install assertion for this one: unlike the
		// downloader it is not a console script we resolve by name, and
		// flashinfer's own import would fail loudly at engine start.
		"ninja",
		TransformersConstraint,
	}
	pipArgs = append(pipArgs, opts.ExtraPipPackages...)
	if err := i.runCapturing(ctx, uvBin, pipArgs, uvEnv, onProgress, StagePipInstall, 3, totalStages, pipBytes); err != nil {
		i.maybeRollback(versionDir, opts.KeepFailed, ours)
		return InstallResult{}, fmt.Errorf("vllm install: uv pip install: %w", err)
	}

	// Stage 4: verify.
	onProgress(InstallProgress{Stage: StageVerify, Step: 4, Total: totalStages, Percent: -1, Message: "verifying: vllm and torch import, the GPU is usable, and the venv can download weights..."})
	pythonBin := filepath.Join(venvDir, "bin", "python")
	if err := i.runCapturing(ctx, pythonBin, []string{"-c", VLLMVerifyImports}, uvEnv, onProgress, StageVerify, 4, totalStages, nil); err != nil {
		i.maybeRollback(versionDir, opts.KeepFailed, ours)
		return InstallResult{}, fmt.Errorf("vllm install: verify: %w", err)
	}

	// Stage 5: activate (swap `current` symlink).
	onProgress(InstallProgress{Stage: StageActivate, Step: 5, Total: totalStages, Percent: 100, Message: "activating: " + filepath.Join(i.BaseDir, "current") + " → " + version})
	// Record the set BEFORE the symlink swap: the record is what the
	// converge reads to decide this venv is up to date, and a venv that
	// is live without one reads as an install that predates the record
	// and is left alone (#843). Written first, the two states a crash can
	// leave are "recorded but not live" (harmless) and "live and
	// recorded" — never "live, claiming nothing".
	if err := writeVLLMPins(versionDir, VLLMPinSet{
		VLLM: version, HFTransfer: hf, Transformers: TransformersConstraint, Python: py,
	}); err != nil {
		return InstallResult{}, fmt.Errorf("vllm install: record pins: %w", err)
	}
	if err := i.activate(version); err != nil {
		return InstallResult{}, fmt.Errorf("vllm install: activate: %w", err)
	}

	return InstallResult{
		Version:     version,
		VenvPath:    venvDir,
		BinDir:      filepath.Join(venvDir, "bin"),
		InstalledAt: i.now(),
	}, nil
}

// Uninstall removes one installed version. If it was the `current`
// version, the symlink is dropped too (Active() then returns ok=false
// and the bootstrap falls back to ollama).
func (i *VLLMInstaller) Uninstall(_ context.Context, version string) error {
	if version == "" {
		return errors.New("vllm install: version required")
	}
	versionDir := filepath.Join(i.BaseDir, version)
	if _, err := os.Stat(versionDir); err != nil {
		return fmt.Errorf("vllm install: %s not present: %w", versionDir, err)
	}
	current, ok := i.Active()
	if ok && filepath.Base(filepath.Dir(current.VenvPath)) == version {
		if err := os.Remove(filepath.Join(i.BaseDir, "current")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("vllm install: drop current symlink: %w", err)
		}
	}
	if err := os.RemoveAll(versionDir); err != nil {
		return fmt.Errorf("vllm install: remove %s: %w", versionDir, err)
	}
	return nil
}

// ErrVLLMNotInstalled means there is genuinely no active install here —
// no `current` symlink, or nothing behind it. It is the ONLY error that
// means "install it"; every other error from ActiveErr describes an
// install that exists and cannot be used as-is.
var ErrVLLMNotInstalled = errors.New("runtime: no active vLLM install")

// Active reads the `current` symlink and returns the active install, or
// ok=false when no install is usable. The boolean façade over ActiveErr,
// kept because most callers only branch on "can I serve on vLLM".
//
// Callers that REPORT to a person should prefer ActiveErr: ok=false here
// covers both "absent" and "present but this process cannot read it", and
// those want opposite advice.
func (i *VLLMInstaller) Active() (InstallResult, bool) {
	res, err := i.ActiveErr()
	return res, err == nil
}

// ActiveErr is Active with the reason it said no.
//
// The distinction exists because collapsing it cost a whole rc9
// verification cycle (waired-agent#778): the venv was complete, but its
// bin/python symlinked into the installing root user's home, so the
// unprivileged daemon's os.Stat failed with EACCES. That is indistinguishable
// from ENOENT through a bool, so the engine decision reported "no engine
// viable", the setup projection reported engine_installed=false, and
// `waired init` waited forever for an engine that was already on disk.
// Same shape as #67, where a probe that could not run answered "no GPU".
func (i *VLLMInstaller) ActiveErr() (InstallResult, error) {
	link := filepath.Join(i.BaseDir, "current")
	target, err := os.Readlink(link)
	if err != nil {
		if os.IsNotExist(err) {
			return InstallResult{}, fmt.Errorf("%w (no %s)", ErrVLLMNotInstalled, link)
		}
		return InstallResult{}, fmt.Errorf("runtime: cannot read %s: %w", link, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(i.BaseDir, target)
	}
	venv := filepath.Join(target, ".venv")
	python := filepath.Join(venv, "bin", "python")
	if _, err := os.Stat(python); err != nil {
		if os.IsNotExist(err) {
			return InstallResult{}, fmt.Errorf("%w (no interpreter at %s)", ErrVLLMNotInstalled, python)
		}
		// Present but unusable. Name the running user: on the daemon this
		// is the service account, and "root installed it, waired cannot
		// read it" is the whole diagnosis in one line.
		return InstallResult{}, fmt.Errorf(
			"runtime: vLLM is installed but this process (uid %d) cannot use %s: %w",
			os.Geteuid(), python, err)
	}
	return InstallResult{
		Version:  filepath.Base(target),
		VenvPath: venv,
		BinDir:   filepath.Join(venv, "bin"),
	}, nil
}

// vllmPinsFile is the name of the record written beside a venv. Inside
// the VERSION directory, not BaseDir, so it is removed by the same
// os.RemoveAll that removes a rolled-back or pruned venv and can never
// outlive the thing it describes.
const vllmPinsFile = "pins.json"

func writeVLLMPins(versionDir string, set VLLMPinSet) error {
	// Encoder rather than MarshalIndent, for SetEscapeHTML(false). The
	// default escapes < and >, so TransformersConstraint lands as
	// "transformers>=5.5.3,<6.0" — it round-trips, but this
	// file is read by a person diagnosing a host whose converge did or
	// did not fire, and the constraint is the field they came for.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(set); err != nil {
		return err
	}
	// 0644: the daemon runs as an unprivileged service user and reads
	// this to decide whether to converge, while the install that wrote it
	// may have run under sudo (#525).
	return os.WriteFile(filepath.Join(versionDir, vllmPinsFile), buf.Bytes(), 0o644)
}

// ActivePins returns the pin set the ACTIVE venv was built from.
//
// ok=false means this install predates the record (#843) or its record
// cannot be read — both of which the converge treats the same way, as
// "no evidence of drift", because the alternative is a ~6 GB rebuild
// triggered by a missing file.
func (i *VLLMInstaller) ActivePins() (VLLMPinSet, bool) {
	active, err := i.ActiveErr()
	if err != nil {
		return VLLMPinSet{}, false
	}
	// VenvPath is <versionDir>/.venv; the record sits beside it.
	b, err := os.ReadFile(filepath.Join(filepath.Dir(active.VenvPath), vllmPinsFile))
	if err != nil {
		return VLLMPinSet{}, false
	}
	var set VLLMPinSet
	if err := json.Unmarshal(b, &set); err != nil {
		return VLLMPinSet{}, false
	}
	return set, true
}

// PruneOtherVersions removes every installed venv except the active one,
// and reports what it removed.
//
// It exists because the converge installs into a NEW version directory
// and swaps a symlink — safe for a serving host, and the reason a vLLM
// converge never has to stop an engine mid-answer, but it means each pin
// move otherwise leaves another ~6 GB on disk for ever. Ollama has no
// equivalent: it replaces one binary in place (#843).
//
// A directory qualifies only when it holds a `.venv`, which is what
// keeps the walk away from `python` — the uv-managed interpreter tree
// that #778 put under BaseDir precisely so it is SHARED between
// versions, and deleting it would break the venv being kept.
//
// `.failed-<ts>` directories are skipped by name, not by shape:
// maybeRollback renames the whole half-built directory, `.venv` and all,
// so they look exactly like a version directory. They were retained by
// an explicit InstallOpts.KeepFailed for someone to inspect, and this is
// not the thing that decides they have been inspected.
//
// Errors are returned but the walk continues: a directory the current
// user cannot remove (a venv built under sudo, before the state dir was
// handed to the service user) must not stop the rest.
func (i *VLLMInstaller) PruneOtherVersions() ([]string, error) {
	active, err := i.ActiveErr()
	if err != nil {
		// Nothing is active: pruning "everything else" would be
		// pruning everything. Refuse rather than guess.
		return nil, fmt.Errorf("vllm prune: %w", err)
	}
	keep := filepath.Base(filepath.Dir(active.VenvPath))
	entries, err := os.ReadDir(i.BaseDir)
	if err != nil {
		return nil, fmt.Errorf("vllm prune: read %s: %w", i.BaseDir, err)
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep || strings.Contains(e.Name(), ".failed-") {
			continue
		}
		dir := filepath.Join(i.BaseDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, ".venv")); err != nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
			continue
		}
		removed = append(removed, e.Name())
	}
	return removed, errors.Join(errs...)
}

// runCapturing runs binary with args, forwarding parsed progress
// events. Lines that look like uv/pip percent updates surface
// through onProgress; everything else flows into the message field
// for diagnostic logging.
//
// bytes, when non-nil, also reads uv's download announcements off the
// same lines and rides its running totals on every event of the stage —
// so the row the wizard is drawing keeps its figures while unrelated
// output flows past (waired-agent#255). Only pip-install passes one;
// the other stages transfer nothing worth a bar.
func (i *VLLMInstaller) runCapturing(ctx context.Context, binary string, args, env []string, onProgress func(InstallProgress), stage InstallStage, step, total int, bytes *uvDownloadTracker) error {
	return i.Runner.Run(ctx, binary, args, env, func(line string) {
		if line == "" {
			return
		}
		p := InstallProgress{
			Stage:   stage,
			Step:    step,
			Total:   total,
			Percent: extractInstallPercent(line),
			Message: line,
		}
		if bytes != nil {
			bytes.Observe(line, i.now())
			p.CompletedBytes, p.TotalBytes, p.BytesPerSec = bytes.Snapshot()
		}
		onProgress(p)
	})
}

// activate atomically swaps `current` → `<version>`. Uses
// rename-over-symlink semantics: write a temp symlink, then
// os.Rename on top of the existing one (POSIX atomic for symlinks
// when both live in the same directory).
func (i *VLLMInstaller) activate(version string) error {
	link := filepath.Join(i.BaseDir, "current")
	tmpLink := link + ".tmp"
	_ = os.Remove(tmpLink)
	if err := os.Symlink(version, tmpLink); err != nil {
		return fmt.Errorf("create temp symlink: %w", err)
	}
	if err := os.Rename(tmpLink, link); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("rename symlink: %w", err)
	}
	return nil
}

// maybeRollback removes the half-built versionDir. When KeepFailed
// is true the directory is renamed to ".failed-<ts>" instead so the
// operator can inspect it.
// venvInterpreter reports whether venvDir holds an interpreter this
// installer can reuse. The same file ActiveErr stats, for the same
// reason: a directory is not an environment.
func venvInterpreter(venvDir string) bool {
	_, err := os.Stat(filepath.Join(venvDir, "bin", "python"))
	return err == nil
}

// maybeRollback cleans up after a failed install — but only when this
// call is what created the version directory (`ours`).
//
// It used to remove it unconditionally, which meant a failed re-entry
// into an EXISTING install deleted a working environment: on a real host
// a converge that stopped at `uv venv` took the venv the machine was
// serving from with it, leaving a dangling `current` (#843). "The
// half-built venv is removed so the next attempt starts clean" is only
// true of a venv this call half-built.
//
// When it is not ours, the directory is left exactly as found. A
// Recreate that failed after --clear leaves an empty environment behind,
// which the next attempt clears again.
func (i *VLLMInstaller) maybeRollback(versionDir string, keep, ours bool) {
	if !ours {
		return
	}
	if !keep {
		_ = os.RemoveAll(versionDir)
		return
	}
	stamp := i.now().Format("20060102-150405")
	failedDir := versionDir + ".failed-" + stamp
	_ = os.Rename(versionDir, failedDir)
}

func (i *VLLMInstaller) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// extractInstallPercent reuses extractPercent's permissive whole-
// number-percent matcher (download/ollama.go) but is duplicated here
// to avoid a cross-package import. Pip / uv emit percentages in many
// shapes ("Downloading torch (700M) 47%", "Resolving deps... 12%").
//
// Returns -1 when no whole-number NN% (1–3 digits, not fractional)
// is present.
func extractInstallPercent(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		// Walk backwards collecting up to 4 digits so we can reject
		// 4-digit runs (1234% is nonsensical).
		j := i - 1
		var digits []byte
		for j >= 0 && s[j] >= '0' && s[j] <= '9' && len(digits) < 4 {
			digits = append([]byte{s[j]}, digits...)
			j--
		}
		if len(digits) == 0 || len(digits) > 3 {
			continue
		}
		// Reject preceding digit or '.' so "99.9%" doesn't read as 9
		// and a 4+ digit run doesn't slip through after the cap.
		if j >= 0 && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			continue
		}
		n, err := strconv.Atoi(string(digits))
		if err != nil || n < 0 || n > 100 {
			continue
		}
		return n
	}
	return -1
}

// DefaultInstallRunner shells out to the real binary, splitting on
// '\n' and '\r' (uv's progress uses '\r' to update the same line).
type DefaultInstallRunner struct{}

func (DefaultInstallRunner) Run(ctx context.Context, binary string, args, env []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", binary, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	scan := func(r interface{ Read([]byte) (int, error) }) {
		defer wg.Done()
		buf := make([]byte, 4096)
		var carry []byte
		for {
			n, err := r.Read(buf)
			if n > 0 {
				carry = append(carry, buf[:n]...)
				for {
					idx := indexAnyByte(carry, "\n\r")
					if idx < 0 {
						break
					}
					line := strings.TrimSpace(string(carry[:idx]))
					if line != "" {
						onLine(line)
					}
					carry = carry[idx+1:]
				}
			}
			if err != nil {
				if line := strings.TrimSpace(string(carry)); line != "" {
					onLine(line)
				}
				return
			}
		}
	}
	go scan(stderr)
	go scan(stdout)
	wg.Wait()
	return cmd.Wait()
}

func indexAnyByte(buf []byte, chars string) int {
	for i, b := range buf {
		for _, c := range []byte(chars) {
			if b == c {
				return i
			}
		}
	}
	return -1
}

// defaultVLLMBaseDir returns $XDG_DATA_HOME/waired/runtimes/vllm
// (or $HOME/.local/share/waired/runtimes/vllm).
func defaultVLLMBaseDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "waired", "runtimes", "vllm")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "waired", "runtimes", "vllm")
}
