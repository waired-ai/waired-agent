package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// DeployOptions controls phase 2.
type DeployOptions struct {
	StateDir string
	// AllowMutations gates the side-effecting parts of deploy. When
	// false, Deploy only resolves the local Ollama install and echoes
	// the planned bundled-model id; cmd/waired/runInit sets it to true
	// so installs pre-pull the bundled weights.
	AllowMutations bool
	// Inference is the agentconfig sub-tree we use to learn preferred
	// ports, bundled model id, etc.
	Inference agentconfig.InferenceConfig
	// ProgressSink, when non-nil, receives one event per pull progress
	// update. Production callers (cmd/waired) supply a rate-limited
	// stdout printer; tests inject a capture buffer.
	ProgressSink func(PullEvent)
	// PullCtxTimeout overrides the per-pull context budget. Zero
	// selects the production default (30 min), long enough that a
	// ~5 GB Q4 GGUF over a poor home link still completes inside the
	// installer.
	PullCtxTimeout time.Duration
	// PullerFactory is a test seam. Production leaves this nil and
	// Deploy constructs `download.NewPuller(binary, DefaultRunner{},
	// "OLLAMA_HOST=...")`.
	PullerFactory func(binary string) Puller
	// EngineProbe is a test seam: reports whether an ollama engine is
	// answering at baseURL. Production leaves it nil (a real HTTP GET
	// /api/tags with a short timeout).
	EngineProbe func(ctx context.Context, baseURL string) bool
}

// Puller is the seam Deploy uses to invoke `ollama pull`. The
// production type (*download.Puller) satisfies it directly.
type Puller interface {
	Pull(ctx context.Context, tag string, onProgress func(download.Progress)) error
}

// PullEvent is one bundled-model download progress update delivered to
// ProgressSink. It carries the human-friendly model name alongside the
// raw download.Progress so the CLI can render a single labelled,
// aggregated download bar (the embedded Progress exposes Digest /
// Completed / Total / BytesPerSec for cross-layer aggregation).
type PullEvent struct {
	ModelName string
	download.Progress
}

// DeployResult is the output Init prints after phase 2.
type DeployResult struct {
	OllamaInstalled bool
	OllamaPath      string
	// OllamaVersion is the raw `ollama --version` string of a detected
	// existing install ("" when not installed / unreadable). Drives the
	// `waired init` bundled-vs-reuse prompt (#188).
	OllamaVersion string
	// OllamaVersionSupported is true when OllamaVersion meets waired's
	// supported floor. False does NOT block reuse — it only adds a
	// warning to the prompt; bundled stays the default either way.
	OllamaVersionSupported bool
	BundledModel           string
	GatewayPort            int
	Notes                  []string
}

// Deploy runs phase 2 (steps 5–9 of spec §5.1): hardware profile,
// runtime/model placement, gateway readiness.
//
// When AllowMutations is true and a viable ollama binary is on PATH,
// Deploy pre-pulls the bundled model so waired-agent's first boot does
// not race a multi-GB download. Pull failures are converted to notes
// and do NOT abort init — the agent will retry on next boot via its
// PullOnStartup path, and the installer surfaces the retry command.
func Deploy(ctx context.Context, opts DeployOptions) (*DeployResult, error) {
	res := &DeployResult{
		BundledModel: opts.Inference.BundledModelID,
		GatewayPort:  opts.Inference.LocalGatewayPort,
	}
	det := DetectOllama(ctx)
	res.OllamaInstalled = det.Installed
	res.OllamaPath = det.Path
	res.OllamaVersion = det.Version
	res.OllamaVersionSupported = det.Supported

	if !opts.AllowMutations {
		if !res.OllamaInstalled {
			res.Notes = append(res.Notes,
				"ollama not found on $PATH — bundled model pre-pull will be deferred until the agent finds it.")
		}
		return res, nil
	}

	// AllowMutations path: try to pre-pull. Every branch ends in a
	// note + nil error so init keeps going.
	switch {
	case !res.OllamaInstalled:
		res.Notes = append(res.Notes, fmt.Sprintf(
			"ollama missing on PATH; cannot pre-pull. Install ollama, "+
				"then run `waired models pull %s`", opts.Inference.BundledModelID))
		return res, nil
	case !opts.Inference.Enabled:
		res.Notes = append(res.Notes,
			"inference disabled by operator choice; skipping bundled-model pre-pull")
		return res, nil
	case !opts.Inference.PullOnStartup:
		res.Notes = append(res.Notes, "pull_on_startup=false; skipping pre-pull")
		return res, nil
	case opts.Inference.BundledModelID == "":
		res.Notes = append(res.Notes, "no bundled_model_id configured; skipping pre-pull")
		return res, nil
	}

	// `ollama pull` is a CLIENT of the serving engine: without one
	// answering on the resolved port the pull cannot land anywhere. In
	// bundled mode the engine is waired-agent's child on the
	// waired-owned port (9475), which typically isn't running during
	// init — the agent pulls on first start instead. (Pre-redesign this
	// pre-pull only ever succeeded by feeding a system ollama on 11434,
	// i.e. the wrong store.)
	engineURL := fmt.Sprintf("http://127.0.0.1:%d", opts.Inference.ResolvedOllamaPort())
	if !probeEngine(ctx, opts.EngineProbe, engineURL) {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"no ollama engine answering at %s; skipping pre-pull (waired-agent pulls on first start)", engineURL))
		return res, nil
	}

	pullTimeout := opts.PullCtxTimeout
	if pullTimeout == 0 {
		pullTimeout = 30 * time.Minute
	}
	pullCtx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()
	// Honour parent cancellation but keep the pull's own 30 min budget
	// separate from the 12 min enroll-wide ctx.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-stop:
		}
	}()

	if err := deployPullBundled(pullCtx, res, opts); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"ollama pull failed (%v); retry with `waired models pull %s` once the issue is resolved",
			err, opts.Inference.BundledModelID))
	}
	return res, nil
}

// deployPullBundled resolves the bundled manifest's ollama tag and
// drives `ollama pull <tag>`. Errors propagate to the caller so Deploy
// can surface a single retry hint in DeployResult.Notes.
func deployPullBundled(ctx context.Context, res *DeployResult, opts DeployOptions) error {
	manifests, err := catalog.BundledManifests()
	if err != nil {
		return fmt.Errorf("load bundled manifests: %w", err)
	}
	manifest, ok := catalog.LookupByAlias(opts.Inference.BundledModelID, manifests)
	if !ok {
		return fmt.Errorf("bundled model %q not in catalog", opts.Inference.BundledModelID)
	}
	tag := ""
	var chosen catalog.Variant
	for _, v := range manifest.Variants {
		if v.Source.Type == catalog.SourceOllama && v.Source.Tag != "" {
			tag = v.Source.Tag
			chosen = v
			break
		}
	}
	if tag == "" {
		return fmt.Errorf("no ollama variant for model %q", manifest.ModelID)
	}

	// Defensive disk pre-flight (#517). The install-time selector
	// (SelectBundledModel) normally steps down or skips on a disk
	// shortfall, but a pinned/forced model — or a reuse store on a
	// different filesystem than the selector probed — can still be short
	// here. Refuse with a clear "insufficient disk" error (Deploy turns it
	// into a retry note) rather than failing mid-download. Best-effort: an
	// unreadable target path doesn't block the pull.
	modelsDir := ollamaModelsDir(opts.Inference.OllamaSource, opts.StateDir, "")
	if free, derr := hardware.FreeDiskBytes(modelsDir); derr == nil {
		required := int64((chosen.EstimatedWeightGB + InstallDiskHeadroomGB) * 1e9)
		if err := download.CheckDiskSpace(free, required); err != nil {
			return fmt.Errorf("disk pre-flight at %s: %w", modelsDir, err)
		}
	}

	puller := newPuller(res.OllamaPath, opts.PullerFactory,
		fmt.Sprintf("OLLAMA_HOST=127.0.0.1:%d", opts.Inference.ResolvedOllamaPort()))
	sink := opts.ProgressSink
	if sink == nil {
		sink = func(PullEvent) {}
	}
	modelName := opts.Inference.BundledModelID
	err = puller.Pull(ctx, tag, func(pr download.Progress) {
		sink(PullEvent{ModelName: modelName, Progress: pr})
	})
	if err != nil {
		// Convert context-isolation cancel into a clearer message.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("pull timed out after %v", opts.PullCtxTimeout)
		}
		return err
	}
	return nil
}

// OllamaDetection summarises a pre-existing ollama install. Used by the
// `waired init` bundled-vs-reuse prompt (#188) and by Deploy.
type OllamaDetection struct {
	Installed bool
	Path      string
	Version   string // raw `ollama --version` token, e.g. "0.24.0"; "" if unknown
	Supported bool   // Version >= OllamaSupportedMinVersion
}

// DetectOllama resolves a pre-existing ollama and (best-effort) its
// version. It never errors: an absent or unreadable ollama yields a
// zero-value (Installed=false) detection.
//
// Resolution goes through download.ResolveBinary so init's detection
// matches what waired-agent's runtime will actually find at boot:
// $WAIRED_OLLAMA_BINARY, then $PATH, then OS well-known install paths.
// The last step matters on macOS (the Ollama.app GUI install lands at
// /Applications/Ollama.app/Contents/Resources/ollama and is NOT on
// $PATH unless the user runs "Install command line") and on Windows
// (a LocalSystem service does not inherit a user PATH). A plain
// exec.LookPath there falsely reports "ollama missing" and skips the
// bundled-model pre-pull. (#268)
func DetectOllama(ctx context.Context) OllamaDetection {
	path, err := download.ResolveBinary("")
	if err != nil {
		return OllamaDetection{}
	}
	ver := detectOllamaVersion(ctx, path)
	return OllamaDetection{
		Installed: true,
		Path:      path,
		Version:   ver,
		Supported: ver != "" && infruntime.OllamaVersionAtLeast(ver, infruntime.OllamaSupportedMinVersion),
	}
}

// detectOllamaVersion runs `<path> --version` and returns the trimmed
// version string (e.g. "0.24.0"), or "" on any error. Best-effort: the
// init prompt only uses it to decide whether to show an "unsupported"
// warning, never to block.
func detectOllamaVersion(ctx context.Context, path string) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	// Output is typically "ollama version is 0.24.0"; keep the last
	// whitespace-separated token of the first non-empty line.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.LastIndex(line, " "); i >= 0 {
			return strings.TrimSpace(line[i+1:])
		}
		return line
	}
	return ""
}

// newPuller delegates to the optional factory or falls back to the
// production `download.NewPuller(...)`. Kept private — the factory hook
// is meant only for unit tests.
func newPuller(binary string, factory func(binary string) Puller, env ...string) Puller {
	if factory != nil {
		return factory(binary)
	}
	return download.NewPuller(binary, download.DefaultRunner{}, env...)
}

// probeEngine reports whether an ollama engine answers at baseURL,
// delegating to the test seam when set.
func probeEngine(ctx context.Context, probe func(context.Context, string) bool, baseURL string) bool {
	if probe != nil {
		return probe(ctx, baseURL)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
