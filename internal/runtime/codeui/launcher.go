package codeui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/opencode"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// launcher.go runs the bundled coding agent ON THE USER SIDE (#486): as the
// invoking user, with the child cwd set to the user's REAL project, behind the
// authenticating proxy in proxy.go. It replaces the old daemon-supervised,
// scratch-workspace model — the hardened root daemon (ProtectHome,
// NoNewPrivileges) structurally cannot edit the user's home.
//
// Process model: `waired codeui open` (or the tray) ensures a detached
// `waired codeui serve` host is running and returns its capability URL.
// `serve` supervises opencode + the proxy and survives an SSH disconnect; a
// per-user runtime.json (0600) records {pid, proxy_addr, token, …} so reopen
// is idempotent and `stop`/`status` can find it.

// Bind/auth mode strings (also the runtime.json values).
const (
	BindLoopback = "loopback"
	BindOverlay  = "overlay"
	AuthToken    = "token"
	AuthBasic    = "basic"

	defaultGatewayBaseURL = "http://127.0.0.1:9473"
	defaultMgmtBaseURL    = "http://127.0.0.1:9476"
	defaultBasicUser      = "waired"

	// EnvBinaryOverride points the installer at an already-present opencode
	// binary (dev / offline / forks), skipping the download.
	EnvBinaryOverride = "WAIRED_CODEUI_BINARY"
	// EnvBasicUser / EnvBasicPassword pin the --auth basic credential instead
	// of generating one. Passed via env (not argv) so it never shows in `ps`.
	EnvBasicUser     = "WAIRED_CODEUI_BASIC_USER"
	EnvBasicPassword = "WAIRED_CODEUI_BASIC_PASSWORD"
)

// Options configure a launch. Zero values resolve to safe defaults.
type Options struct {
	Project        string // child cwd: the real project the agent edits
	Bind           string // BindLoopback (default) | BindOverlay | explicit host/IP
	Auth           string // AuthToken (default) | AuthBasic
	Port           int    // front proxy port; 0 → DefaultCodeUIPort (9480)
	GatewayBaseURL string // local gateway base; "" → defaultGatewayBaseURL
	MgmtBaseURL    string // loopback mgmt API (overlay-IP discovery); "" → default
	BaseDir        string // per-user codeui dir; "" → DefaultBaseDir()
	Logger         *slog.Logger
}

// RuntimeInfo is the per-user runtime.json: the live instance's address +
// secrets. Mode 0600 — it carries the capability token / basic password.
type RuntimeInfo struct {
	PID         int    `json:"pid"`
	ProxyAddr   string `json:"proxy_addr"`   // host:port the proxy listens on
	BackendPort int    `json:"backend_port"` // ephemeral opencode loopback port
	Project     string `json:"project"`
	Bind        string `json:"bind"`
	Auth        string `json:"auth"`
	Token       string `json:"token,omitempty"`      // capability token (AuthToken)
	BasicUser   string `json:"basic_user,omitempty"` // AuthBasic
	BasicPass   string `json:"basic_pass,omitempty"` // AuthBasic
	StartedUnix int64  `json:"started_unix"`
	Version     string `json:"opencode_version"`
}

// URL is the address to open. In token mode it embeds the capability token so
// the tray/CLI launch is friction-free; in basic mode the browser prompts.
func (r *RuntimeInfo) URL() string {
	base := "http://" + r.ProxyAddr + "/"
	if r.Auth == AuthToken && r.Token != "" {
		return base + "?" + CapabilityTokenParam + "=" + r.Token
	}
	return base
}

// DefaultBaseDir is the per-user codeui directory:
// <per-user-state-dir>/runtimes/codeui (e.g. ~/.config/waired/runtimes/codeui
// on Linux). User-owned, so opencode runs and writes as the user.
func DefaultBaseDir() string {
	return filepath.Join(paths.StateDir(paths.Interactive), "runtimes", "codeui")
}

func (o *Options) withDefaults() {
	if o.Bind == "" {
		o.Bind = BindLoopback
	}
	if o.Auth == "" {
		o.Auth = AuthToken
	}
	if o.Port == 0 {
		o.Port = DefaultCodeUIPort
	}
	if o.GatewayBaseURL == "" {
		o.GatewayBaseURL = defaultGatewayBaseURL
	}
	if o.MgmtBaseURL == "" {
		o.MgmtBaseURL = defaultMgmtBaseURL
	}
	if o.BaseDir == "" {
		o.BaseDir = DefaultBaseDir()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// DefaultCodeUIPort is the front-door (proxy) port the bundled coding-agent UI
// is served at.
const DefaultCodeUIPort = 9480

// Open ensures a detached `serve` host is running for opts and returns its
// RuntimeInfo. Idempotent: a live instance serving the same project/bind/auth
// is reused; a mismatch restarts onto the new request. This is the entry point
// for `waired codeui open` and the tray.
func Open(ctx context.Context, opts Options) (*RuntimeInfo, error) {
	opts.withDefaults()
	if info, ok := liveInstance(opts.BaseDir); ok {
		if instanceMatches(info, opts) {
			return info, nil
		}
		opts.Logger.Info("codeui: restarting onto a new project/bind",
			"old", info.Project, "new", opts.Project)
		_ = Stop(opts)
	}
	if err := spawnServe(opts); err != nil {
		return nil, err
	}
	return waitForInstance(ctx, opts)
}

// Status returns the live instance (if any).
func Status(opts Options) (*RuntimeInfo, bool) {
	opts.withDefaults()
	return liveInstance(opts.BaseDir)
}

// Stop terminates the running instance and clears its runtime file.
func Stop(opts Options) error {
	opts.withDefaults()
	info, ok := readRuntime(opts.BaseDir)
	if !ok {
		return nil
	}
	if info.PID > 0 {
		signalStop(info.PID)
	}
	removeRuntime(opts.BaseDir)
	return nil
}

// Serve is the long-running host: install + seed + start opencode + proxy,
// publish runtime.json, then block until ctx is cancelled (SIGTERM). It is the
// body of the detached `waired codeui serve` process.
func Serve(ctx context.Context, opts Options) error {
	opts.withDefaults()
	log := opts.Logger

	// Another host already owns this user's instance — Open should have reused
	// it. Exit cleanly rather than fight for the port.
	if info, ok := liveInstance(opts.BaseDir); ok && instanceMatches(info, opts) {
		log.Info("codeui serve: instance already running; nothing to do", "url", info.URL())
		return nil
	}

	installer := NewInstaller(opts.BaseDir)
	if b := os.Getenv(EnvBinaryOverride); b != "" {
		installer.LocalBinary = b
	}
	if installer.NeedsInstall() {
		log.Info("codeui: installing bundled coding-agent runtime (first run downloads opencode)",
			"installed", installer.InstalledVersion(), "pin", OpenCodePinnedVersion)
	}
	if err := installer.Install(ctx, func(p InstallProgress) {
		log.Info("codeui install", "stage", p.Stage, "msg", p.Message)
	}); err != nil {
		return fmt.Errorf("codeui: install: %w", err)
	}

	// Seed the isolated config (opencode reads <XDG_CONFIG_HOME>/opencode/).
	if _, err := WriteDefaultConfig(installer.OpenCodeConfigDir()); err != nil {
		return fmt.Errorf("codeui: seed config: %w", err)
	}
	if _, err := opencode.WritePluginInConfigDir(installer.OpenCodeConfigDir(), opts.GatewayBaseURL); err != nil {
		return fmt.Errorf("codeui: seed provider plugin: %w", err)
	}

	// Backend: ephemeral loopback port + random Basic password. The proxy is
	// the only client; a sibling local user who finds the port gets 401.
	backendPort, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("codeui: pick backend port: %w", err)
	}
	backendPassword, err := GenerateToken()
	if err != nil {
		return err
	}
	svc := New(Config{
		Host:           "127.0.0.1",
		Port:           backendPort,
		BinaryPath:     installer.BinaryPath(),
		XDGConfigHome:  installer.ConfigDir(),
		DataDir:        installer.DataDir(),
		Workspace:      opts.Project,
		ServerPassword: backendPassword,
		LogDir:         installer.LogDir(),
		Spawner:        infruntime.DefaultSpawner{Dir: opts.Project},
	})
	if err := svc.EnsureRunning(ctx); err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Stop(stopCtx)
	}()

	// Front-door auth + the secrets we publish to the launcher.
	info := &RuntimeInfo{
		PID:         os.Getpid(),
		BackendPort: backendPort,
		Project:     opts.Project,
		Bind:        opts.Bind,
		Auth:        opts.Auth,
		StartedUnix: time.Now().Unix(),
		Version:     OpenCodePinnedVersion,
	}
	auth, err := buildAuthenticator(opts.Auth, info)
	if err != nil {
		return err
	}

	frontHost, err := resolveBindHost(opts)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(frontHost, strconv.Itoa(opts.Port)))
	if err != nil {
		return fmt.Errorf("codeui: bind proxy on %s:%d: %w", frontHost, opts.Port, err)
	}
	info.ProxyAddr = ln.Addr().String()
	backendAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort))
	srv := &http.Server{Handler: ProxyHandler(backendAddr, backendPassword, auth)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	if err := writeRuntime(opts.BaseDir, info); err != nil {
		_ = srv.Close()
		return err
	}
	defer removeRuntime(opts.BaseDir)
	log.Info("codeui ready", "url", info.URL(), "project", opts.Project,
		"bind", opts.Bind, "auth", auth.Mode())

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("codeui: proxy serve: %w", err)
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return nil
}

func buildAuthenticator(mode string, info *RuntimeInfo) (Authenticator, error) {
	switch mode {
	case AuthBasic:
		user := envOr(EnvBasicUser, defaultBasicUser)
		pass := os.Getenv(EnvBasicPassword)
		if pass == "" {
			gen, err := GenerateToken()
			if err != nil {
				return nil, err
			}
			pass = gen[:16] // shorter, still 64 bits of entropy, easier to relay
		}
		info.BasicUser = user
		info.BasicPass = pass
		return NewBasicAuth(user, pass), nil
	case AuthToken, "":
		tok, err := GenerateToken()
		if err != nil {
			return nil, err
		}
		info.Token = tok
		info.Auth = AuthToken
		return NewTokenAuth(tok), nil
	default:
		return nil, fmt.Errorf("codeui: unknown --auth %q (want token|basic)", mode)
	}
}

// resolveBindHost maps the bind mode to a concrete host. "overlay" is
// discovered from the loopback mgmt API so only same-waired-network peers
// (WireGuard membership) can reach it; an explicit host/IP passes through.
func resolveBindHost(opts Options) (string, error) {
	switch opts.Bind {
	case "", BindLoopback:
		return "127.0.0.1", nil
	case BindOverlay:
		ip, err := discoverOverlayIP(opts.MgmtBaseURL)
		if err != nil {
			return "", fmt.Errorf("codeui: --bind overlay: %w", err)
		}
		return ip, nil
	default:
		return opts.Bind, nil
	}
}

func discoverOverlayIP(mgmtBaseURL string) (string, error) {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(mgmtBaseURL + "/waired/v1/status")
	if err != nil {
		return "", fmt.Errorf("query agent status: %w (is the waired agent running?)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var st struct {
		OverlayIP string `json:"overlay_ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return "", fmt.Errorf("decode agent status: %w", err)
	}
	if st.OverlayIP == "" {
		return "", errors.New("agent has no overlay IP yet (not enrolled, or the network is still coming up)")
	}
	return st.OverlayIP, nil
}

// --- instance discovery / lifecycle helpers ----------------------------------

func liveInstance(baseDir string) (*RuntimeInfo, bool) {
	info, ok := readRuntime(baseDir)
	if !ok {
		return nil, false
	}
	if !httpAlive(info.ProxyAddr) {
		return nil, false
	}
	return info, true
}

func instanceMatches(info *RuntimeInfo, opts Options) bool {
	return sameProject(info.Project, opts.Project) &&
		info.Bind == opts.Bind && info.Auth == opts.Auth
}

func sameProject(a, b string) bool {
	ap, err1 := filepath.Abs(a)
	bp, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ap == bp
}

// httpAlive reports whether something answers HTTP on addr. Any response —
// including the proxy's 401 — counts as alive; only a connection failure means
// the instance is gone.
func httpAlive(addr string) bool {
	if addr == "" {
		return false
	}
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// spawnServe launches `waired codeui serve <flags>` detached from the calling
// terminal so it survives an SSH logout. Secrets are NOT passed on argv (they
// would show in `ps`); the serve process generates them and publishes them via
// the 0600 runtime.json.
func spawnServe(opts Options) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("codeui: locate waired binary: %w", err)
	}
	args := []string{
		"codeui", "serve",
		"--project", opts.Project,
		"--bind", opts.Bind,
		"--auth", opts.Auth,
		"--port", strconv.Itoa(opts.Port),
		"--gateway-base-url", opts.GatewayBaseURL,
		"--mgmt", opts.MgmtBaseURL,
		"--base-dir", opts.BaseDir,
	}
	if err := os.MkdirAll(filepath.Join(opts.BaseDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("codeui: mkdir logs: %w", err)
	}
	logPath := filepath.Join(opts.BaseDir, "logs", "codeui-serve.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("codeui: open serve log: %w", err)
	}
	defer func() { _ = logf.Close() }()

	cmd := exec.Command(self, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codeui: start serve host: %w", err)
	}
	// Detach: do not Wait — the host outlives us.
	return nil
}

// waitForInstance polls until the detached serve publishes a healthy
// runtime.json (the first run includes a ~55MB download), or fails.
func waitForInstance(ctx context.Context, opts Options) (*RuntimeInfo, error) {
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		if info, ok := liveInstance(opts.BaseDir); ok && instanceMatches(info, opts) {
			return info, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("codeui: coding agent did not come up within 3m (see %s)",
				filepath.Join(opts.BaseDir, "logs", "codeui-serve.log"))
		case <-tick.C:
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
