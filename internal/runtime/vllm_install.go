//go:build linux

// vLLM installer: Linux-only. See vllm.go for the cross-platform
// rationale (Windows / macOS use the stub files).

package runtime

import (
	"context"
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

// VLLMPinnedVersion is the vLLM release Step 2 installs into the
// uv-managed venv. Refreshed together with VLLMVerifyImports's
// SM-capability check whenever upstream drops a new Blackwell-aware
// build. Bumping is a documented Step 8 / 14 maintenance task.
// renovate: datasource=pypi depName=vllm
const VLLMPinnedVersion = "0.24.0"

// HFTransferPinnedVersion is the hf_transfer wheel installed alongside
// vLLM so HF downloads enable the Rust fast path.
// renovate: datasource=pypi depName=hf_transfer
const HFTransferPinnedVersion = "0.1.9"

// TransformersConstraint pins the transformers wheel to a version
// compatible with VLLMPinnedVersion. vllm 0.24.0 requires
// transformers>=5.5.3 (its 0.11-era code needed <5.0 instead — the two
// major lines are mutually incompatible), so the constraint here only
// caps the major to keep uv from resolving a future transformers 6.x
// before it has been verified. Bump together with VLLMPinnedVersion
// after verifying compatibility on a real GPU host.
const TransformersConstraint = "transformers>=5.5.3,<6.0"

// VLLMVerifyImports is the python snippet the install pipeline runs
// to confirm the venv is truly usable: vllm importable, torch sees
// CUDA, GPU compute capability ≥ 10 (= Blackwell-ready). Step 14 may
// extend this with additional smoke-checks.
const VLLMVerifyImports = `
import sys, vllm, torch
assert torch.cuda.is_available(), 'torch.cuda.is_available() is False'
cap = torch.cuda.get_device_capability(0)
if cap[0] < 8:
    sys.exit(f'compute capability {cap[0]}.{cap[1]} below the SM_80 floor')
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
	Stage   InstallStage
	Step    int
	Total   int
	Percent int // 0-100 when the underlying tool reports it; -1 otherwise
	Message string
}

// InstallResult is the venv that Install successfully materialised.
type InstallResult struct {
	Version     string
	VenvPath    string
	BinDir      string
	InstalledAt time.Time
}

// InstallOpts customises what gets installed. Defaults pin to
// VLLMPinnedVersion / HFTransferPinnedVersion and Python 3.12 (the
// Step 2 supported interpreter window).
type InstallOpts struct {
	Version           string
	HFTransferVersion string
	PythonVersion     string // e.g. "3.12"
	KeepFailed        bool   // leave the broken venv in place under ".failed-<ts>"
	ExtraPipPackages  []string
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

// Install builds (or rebuilds) the venv for opts.Version. Idempotent
// for an already-installed version when its venv exists and verify
// passes; otherwise the venv is rebuilt from scratch. On failure the
// half-built venv is removed (or relocated to .failed-<ts> when
// KeepFailed is set) so the next attempt starts clean.
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
		py = "3.12"
	}
	const totalStages = 5

	// Stage 1: resolve uv.
	onProgress(InstallProgress{Stage: StageResolveUV, Step: 1, Total: totalStages, Percent: -1, Message: "resolving uv binary..."})
	uvBin, err := i.UV.Resolve(ctx, "")
	if err != nil {
		return InstallResult{}, fmt.Errorf("vllm install: %w", err)
	}

	versionDir := filepath.Join(i.BaseDir, version)
	venvDir := filepath.Join(versionDir, ".venv")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("vllm install: mkdir version dir: %w", err)
	}

	// Stage 2: create venv.
	onProgress(InstallProgress{Stage: StageCreateVenv, Step: 2, Total: totalStages, Percent: -1, Message: "creating venv (Python " + py + ")..."})
	// `uv venv --python 3.12 <dir>` creates a fresh venv. If <dir>
	// already exists with the same interpreter, uv exits successfully
	// without rebuilding, which gives us idempotency for free.
	if err := i.runCapturing(ctx, uvBin, []string{"venv", "--python", py, venvDir}, nil, onProgress, StageCreateVenv, 2, totalStages); err != nil {
		i.maybeRollback(versionDir, opts.KeepFailed)
		return InstallResult{}, fmt.Errorf("vllm install: uv venv: %w", err)
	}

	// Stage 3: pip install. Pass --python so uv doesn't infer from PATH.
	onProgress(InstallProgress{Stage: StagePipInstall, Step: 3, Total: totalStages, Percent: -1, Message: "installing vllm==" + version + " hf_transfer==" + hf + " (this may take 5-15 minutes, ~6 GB)..."})
	pipArgs := []string{
		"pip", "install",
		"--python", filepath.Join(venvDir, "bin", "python"),
		"vllm==" + version,
		"hf_transfer==" + hf,
		// huggingface_hub ships the `hf` / `huggingface-cli` binary the
		// agent's HFPuller (internal/download/hf.go, ResolveHFCLI) shells
		// out to for the safetensors download. vLLM already pulls
		// huggingface_hub in transitively; the explicit [cli] extra makes
		// the console-script entry point a hard guarantee rather than a
		// transitive accident, so a resolver change upstream can't leave
		// the venv without a downloader. Version is left to uv so it
		// resolves the one vllm pins.
		"huggingface_hub[cli]",
		// vllm 0.24's flashinfer JIT-compiles CUDA ops at engine
		// start-up and shells out to `ninja`. The wheel arrives
		// transitively today, but pin it explicitly for the same
		// reason as huggingface_hub above: a resolver change must
		// not leave the venv unable to serve. (VLLMAdapter puts the
		// venv bin dir on the child PATH so this binary is found.)
		"ninja",
		TransformersConstraint,
	}
	pipArgs = append(pipArgs, opts.ExtraPipPackages...)
	if err := i.runCapturing(ctx, uvBin, pipArgs, nil, onProgress, StagePipInstall, 3, totalStages); err != nil {
		i.maybeRollback(versionDir, opts.KeepFailed)
		return InstallResult{}, fmt.Errorf("vllm install: uv pip install: %w", err)
	}

	// Stage 4: verify.
	onProgress(InstallProgress{Stage: StageVerify, Step: 4, Total: totalStages, Percent: -1, Message: "verifying: python -c 'import vllm, torch; assert torch.cuda.is_available()'..."})
	pythonBin := filepath.Join(venvDir, "bin", "python")
	if err := i.runCapturing(ctx, pythonBin, []string{"-c", VLLMVerifyImports}, nil, onProgress, StageVerify, 4, totalStages); err != nil {
		i.maybeRollback(versionDir, opts.KeepFailed)
		return InstallResult{}, fmt.Errorf("vllm install: verify: %w", err)
	}

	// Stage 5: activate (swap `current` symlink).
	onProgress(InstallProgress{Stage: StageActivate, Step: 5, Total: totalStages, Percent: 100, Message: "activating: " + filepath.Join(i.BaseDir, "current") + " → " + version})
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

// Active reads the `current` symlink and returns the active install,
// or ok=false when no install is active.
func (i *VLLMInstaller) Active() (InstallResult, bool) {
	target, err := os.Readlink(filepath.Join(i.BaseDir, "current"))
	if err != nil {
		return InstallResult{}, false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(i.BaseDir, target)
	}
	venv := filepath.Join(target, ".venv")
	if _, err := os.Stat(filepath.Join(venv, "bin", "python")); err != nil {
		return InstallResult{}, false
	}
	version := filepath.Base(target)
	return InstallResult{
		Version:  version,
		VenvPath: venv,
		BinDir:   filepath.Join(venv, "bin"),
	}, true
}

// runCapturing runs binary with args, forwarding parsed progress
// events. Lines that look like uv/pip percent updates surface
// through onProgress; everything else flows into the message field
// for diagnostic logging.
func (i *VLLMInstaller) runCapturing(ctx context.Context, binary string, args, env []string, onProgress func(InstallProgress), stage InstallStage, step, total int) error {
	return i.Runner.Run(ctx, binary, args, env, func(line string) {
		if line == "" {
			return
		}
		pct := extractInstallPercent(line)
		onProgress(InstallProgress{
			Stage:   stage,
			Step:    step,
			Total:   total,
			Percent: pct,
			Message: line,
		})
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
func (i *VLLMInstaller) maybeRollback(versionDir string, keep bool) {
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
