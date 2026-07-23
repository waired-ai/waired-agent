package codeui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// OpenCodeServerUser is the HTTP Basic auth username opencode expects when
// OPENCODE_SERVER_PASSWORD is set. It is opencode's hard-coded default; we do
// not override it via OPENCODE_SERVER_USERNAME.
const OpenCodeServerUser = "opencode"

// Config wires a Service. Zero time fields fall back to sane defaults so
// production code only sets the paths, network, and Spawner.
type Config struct {
	// Host/Port the opencode server binds (loopback only).
	Host string
	Port int

	// BinaryPath is the absolute path to the bundled opencode binary.
	BinaryPath string
	// XDGConfigHome becomes XDG_CONFIG_HOME for the child. opencode reads
	// <XDGConfigHome>/opencode/ for opencode.json + plugin/ — this is the
	// real isolation knob in opencode 1.17.8 (OPENCODE_CONFIG_DIR is
	// silently IGNORED; verified empirically). Pointing it at a waired-owned
	// dir keeps the bundled instance off the user's ~/.config/opencode.
	XDGConfigHome string
	// DataDir backs XDG_DATA_HOME (and, under it, cache/state) so the
	// instance's sessions/auth never land in the user's home.
	DataDir string
	// Workspace is the child's working directory: the project the coding
	// agent operates on (`opencode serve` scopes to its cwd). The launcher
	// also passes it via Spawner.Dir; this field is informational.
	Workspace string
	// ServerPassword, when non-empty, is exported as OPENCODE_SERVER_PASSWORD
	// so opencode guards every route (HTML, REST, and the SSE /event stream —
	// verified) with HTTP Basic auth (username "opencode"). This is the
	// backend hardening: a local user who finds the loopback port still gets
	// 401. The readiness probe authenticates with it.
	ServerPassword string

	// Spawner abstracts the subprocess starter. Production passes
	// infruntime.DefaultSpawner{Dir: Workspace} so the child cwd is the
	// project; tests inject a fake.
	Spawner infruntime.Spawner
	// HTTPClient is used for the readiness probe.
	HTTPClient *http.Client

	// LogDir, when non-empty, captures the server's merged stdout+stderr to
	// <LogDir>/codeui.log so a failed boot leaves a trail.
	LogDir string

	// ExtraEnv augments the child env (tests). Production leaves it empty.
	ExtraEnv []string

	// Timings (zero -> default).
	HealthInterval      time.Duration // probe interval (default 500ms)
	StartupReadyTimeout time.Duration // first-ready budget (default 60s)
	StopTimeout         time.Duration // SIGTERM->SIGKILL grace (default 5s)
}

// Service supervises the bundled `opencode serve` process as a foreground
// child. It is a thin lifecycle wrapper — not an inference Adapter — but
// reuses the runtime.Spawner seam for testability and the same spawn/probe/
// stop shape as OllamaAdapter.
type Service struct {
	cfg     Config
	baseURL string

	mu    sync.Mutex
	state infruntime.Health
	proc  infruntime.RunningProcess
	// procCancel cancels the Service-owned context the child was spawned
	// under. It is deliberately NOT the caller's context (see EnsureRunning)
	// so the process outlives the request that started it; Stop()/a failed
	// startup release it.
	procCancel context.CancelFunc
	logF       *os.File
}

// New constructs a Service, applying defaults.
func New(cfg Config) *Service {
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 500 * time.Millisecond
	}
	if cfg.StartupReadyTimeout <= 0 {
		cfg.StartupReadyTimeout = 60 * time.Second
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	return &Service{
		cfg:     cfg,
		baseURL: fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port),
		state:   infruntime.Health{State: infruntime.StateNotStarted},
	}
}

// URL is the loopback address the coding-agent UI is served at.
func (s *Service) URL() string { return s.baseURL }

// Health returns a snapshot of the current state.
func (s *Service) Health() infruntime.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// EnsureRunning starts `opencode serve` (if not already running) and blocks
// until it is ready or startup fails. Idempotent: a ready service is a no-op.
func (s *Service) EnsureRunning(ctx context.Context) error {
	s.mu.Lock()
	switch s.state.State {
	case infruntime.StateReady:
		s.mu.Unlock()
		return nil
	case infruntime.StateStarting:
		s.mu.Unlock()
		return fmt.Errorf("codeui: EnsureRunning called while already starting")
	}
	s.state = infruntime.Health{State: infruntime.StateStarting}
	s.mu.Unlock()

	if s.cfg.BinaryPath == "" {
		err := fmt.Errorf("codeui: BinaryPath must be set")
		s.setState(infruntime.Health{State: infruntime.StateFailed, LastErr: err.Error()})
		return err
	}

	args := []string{
		"serve",
		"--port", strconv.Itoa(s.cfg.Port),
		"--hostname", s.cfg.Host,
	}
	logW := s.openLog()
	// The child must outlive the ctx that triggered this call. EnsureRunning
	// is reached from the management API handler holding the HTTP *request*
	// context (POST /waired/v1/codeui/open); binding the process to it via
	// exec.CommandContext would SIGKILL opencode the instant that request
	// completes — leaving the Service cached as Ready but nothing listening.
	// Spawn under a Service-owned context cancelled only by Stop() / a failed
	// startup. ctx still governs the readiness wait below.
	procCtx, procCancel := context.WithCancel(context.Background())
	slog.DebugContext(ctx, "codeui service: spawning opencode",
		"binary", s.cfg.BinaryPath, "host", s.cfg.Host, "port", s.cfg.Port)
	proc, err := s.cfg.Spawner.Spawn(procCtx, s.cfg.BinaryPath, args, s.processEnv(), logW)
	if err != nil {
		procCancel()
		s.closeLog()
		s.setState(infruntime.Health{State: infruntime.StateFailed, LastErr: err.Error()})
		return fmt.Errorf("codeui: spawn: %w", err)
	}
	s.mu.Lock()
	s.proc = proc
	s.procCancel = procCancel
	s.mu.Unlock()

	startCtx := ctx
	if s.cfg.StartupReadyTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, s.cfg.StartupReadyTimeout)
		defer cancel()
	}
	if err := s.waitReady(startCtx); err != nil {
		_ = s.stopProcess(context.Background()) // also releases procCancel
		if ctxErr := startCtx.Err(); ctxErr != nil {
			err = fmt.Errorf("codeui: not ready within %s (see %s)", s.cfg.StartupReadyTimeout, s.logPath())
		}
		slog.DebugContext(ctx, "codeui service: opencode failed to become ready", "err", err)
		s.setState(infruntime.Health{State: infruntime.StateFailed, LastErr: err.Error()})
		return err
	}
	slog.DebugContext(ctx, "codeui service: opencode ready", "url", s.baseURL)
	s.setState(infruntime.Health{State: infruntime.StateReady, LastOK: time.Now()})
	return nil
}

// processEnv builds the child environment: the parent env with our keys
// overlaid so the bundled opencode instance is fully isolated from the
// user's ~/.config/opencode and ~/.local/share/opencode.
func (s *Service) processEnv() []string {
	overrides := map[string]string{
		// Config isolation: opencode reads <XDG_CONFIG_HOME>/opencode/ for
		// opencode.json + plugin/waired.js. (1.17.8 ignores OPENCODE_CONFIG_DIR.)
		"XDG_CONFIG_HOME": s.cfg.XDGConfigHome,
		// Data/cache/state isolation: sessions/auth stay under our dir.
		"XDG_DATA_HOME":  s.cfg.DataDir,
		"XDG_CACHE_HOME": filepath.Join(s.cfg.DataDir, "cache"),
		"XDG_STATE_HOME": filepath.Join(s.cfg.DataDir, "state"),
		// Never let the vendored, sha-pinned binary replace itself.
		"OPENCODE_DISABLE_AUTOUPDATE": "1",
	}
	// Backend hardening: a non-empty password makes opencode require HTTP
	// Basic auth on every route, so the waired auth proxy is the only client
	// that can reach it — a sibling local user who finds the loopback port
	// gets 401. The username is opencode's default ("opencode").
	if s.cfg.ServerPassword != "" {
		overrides["OPENCODE_SERVER_PASSWORD"] = s.cfg.ServerPassword
	}
	var env []string
	for _, kv := range os.Environ() {
		if k := envKey(kv); k != "" {
			if _, shadowed := overrides[k]; shadowed {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	// #22-class defense: guarantee a writable HOME (our isolated DataDir) so
	// opencode never dies resolving ~ if ever launched HOME-less. It runs
	// user-side today (HOME normally set, so this is a no-op), but routing
	// through the shared ChildBaseEnv keeps codeui in parity with the
	// ollama/vLLM spawn paths. No PATH augmentation (opencode is resolved by
	// absolute path).
	env = infruntime.ChildBaseEnv(runtime.GOOS, env, s.cfg.DataDir, "", string(os.PathListSeparator))
	env = append(env, s.cfg.ExtraEnv...)
	return env
}

func envKey(kv string) string {
	k, _, ok := strings.Cut(kv, "=")
	if !ok {
		return ""
	}
	return k
}

// waitReady polls GET baseURL/ until it answers 2xx/3xx, failing fast if the
// child exits or the context is cancelled.
func (s *Service) waitReady(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.HealthInterval)
	defer tick.Stop()
	for {
		if s.probeOnce(ctx) {
			return nil
		}
		var procDone <-chan struct{}
		s.mu.Lock()
		proc := s.proc
		s.mu.Unlock()
		if proc != nil {
			procDone = proc.Done()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-procDone:
			return fmt.Errorf("codeui: process exited during startup: %v", proc.Err())
		case <-tick.C:
		}
	}
}

func (s *Service) probeOnce(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/", nil)
	if err != nil {
		return false
	}
	// When the backend is password-protected, the probe must authenticate or
	// it would see a perpetual 401 and never go ready.
	if s.cfg.ServerPassword != "" {
		req.SetBasicAuth(OpenCodeServerUser, s.cfg.ServerPassword)
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// Stop terminates the server gracefully (SIGTERM, then SIGKILL after
// StopTimeout). A never-started Service is a no-op.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.proc == nil {
		s.state = infruntime.Health{State: infruntime.StateStopped}
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	slog.DebugContext(ctx, "codeui service: stopping opencode")
	if err := s.stopProcess(ctx); err != nil {
		s.setState(infruntime.Health{State: infruntime.StateFailed, LastErr: err.Error()})
		return err
	}
	s.setState(infruntime.Health{State: infruntime.StateStopped})
	return nil
}

func (s *Service) stopProcess(ctx context.Context) error {
	s.mu.Lock()
	proc := s.proc
	cancel := s.procCancel
	s.mu.Unlock()
	if proc == nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-proc.Done():
		s.closeLog()
	case <-time.After(s.cfg.StopTimeout):
		slog.Debug("codeui service: opencode did not exit on SIGTERM; killing", "grace", s.cfg.StopTimeout)
		_ = proc.Kill()
		<-proc.Done()
		s.closeLog()
	case <-ctx.Done():
		_ = proc.Kill()
		s.closeLog()
		s.clearProc(cancel)
		return ctx.Err()
	}
	s.clearProc(cancel)
	return nil
}

// clearProc drops the process handle and releases the Service-owned spawn
// context. Safe to call with a nil cancel.
func (s *Service) clearProc(cancel context.CancelFunc) {
	s.mu.Lock()
	s.proc = nil
	s.procCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) setState(h infruntime.Health) {
	s.mu.Lock()
	s.state = h
	s.mu.Unlock()
}

func (s *Service) logPath() string {
	if s.cfg.LogDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.LogDir, "codeui.log")
}

// openLog truncates and opens the per-run log file; nil when LogDir is unset.
func (s *Service) openLog() io.Writer {
	if s.cfg.LogDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.logF = f
	s.mu.Unlock()
	return f
}

func (s *Service) closeLog() {
	s.mu.Lock()
	f := s.logF
	s.logF = nil
	s.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}
