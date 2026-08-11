// Package integration owns the auto-configuration of third-party
// coding agents (Claude Code, OpenClaw, ...) so they route their
// requests through the local Waired Gateway at 127.0.0.1:9473.
//
// The package is structured around a small Adapter interface so adding
// support for a new tool is "drop one file under internal/integration".
// It ships claudecode/ and openclaw/ subpackages; retired/ holds the
// uninstall-only remnants of integrations that have been withdrawn.
//
// Apply mutates the user's home directory (a Waired-managed env.sh and
// a sentinel-bracketed source line in shell rc files for Claude Code;
// a self-contained plugin directory for OpenClaw).
// Apply is idempotent. Uninstall reverts everything Apply touched, by
// reading the ledger written to ~/.config/waired/integrations/applied.json.
//
// Critical design choices (see waired/docs/decisions/):
//
//   - ~/.claude/settings.json is NEVER touched. We rely on the shell
//     environment instead, in pyenv/nvm/conda style.
//   - The gateway token is per-install, generated on first Apply, and
//     stored at ~/.config/waired/secrets/gateway-token (mode 0600).
//   - Every file or directory we create is recorded in the ledger so
//     Uninstall is a precise removal, not a regex sweep.
package integration

import (
	"context"
	"errors"
	"fmt"
)

// AgentID identifies a supported coding agent.
type AgentID string

const (
	AgentClaudeCode AgentID = "claude-code"
	AgentOpenClaw   AgentID = "openclaw"
)

// Status reports whether an item passed audit.
type Status int

const (
	StatusUnknown Status = iota
	StatusOK             // configured and healthy
	StatusWarn           // present but drifted (will be re-applied by --fix)
	StatusFail           // missing or broken
	StatusSkip           // not applicable on this host (agent not installed)
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	default:
		return "unknown"
	}
}

// Detection is the result of an Adapter.Detect call.
type Detection struct {
	// Found is true when the agent looks installed: either its CLI is
	// on $PATH or its well-known config directory exists. waired init
	// only auto-applies adapters whose Found is true; `waired link
	// <agent>` (explicit invocation) bypasses this.
	Found bool
	// BinaryPath, if non-empty, is the resolved path of the agent's
	// CLI binary on $PATH. May be empty even when Found=true (e.g.
	// only the dotfile dir exists).
	BinaryPath string
	// ConfigDir, if non-empty, is the agent's known config directory
	// (typically under $HOME). May be empty when Found=true through
	// $PATH alone (the user has never run the agent yet).
	ConfigDir string
	// Notes carries human-readable context for `waired status` /
	// `waired doctor` to surface to the operator.
	Notes []string
}

// AuditFinding is one line of `waired doctor`-style output.
type AuditFinding struct {
	Status  Status
	Subject string // short label, e.g. "claude-code shell-init"
	Detail  string // longer message, suitable for --details
}

// InstallationFinding renders the "<agent> installation" row every adapter
// ends its audit with. Shared so the two adapters cannot drift: the
// judgement is about Detection, which is a shape all of them produce.
//
// The three states are deliberate. Detect sets Found when EITHER the
// binary is on $PATH OR the config dir has non-waired content, so a host
// with ~/.openclaw but no binary used to pass as
// `✓ openclaw installation — binary= configDir=/home/<user>/.openclaw`
// — a tick with an empty field, asserting an installation that is not
// there (#652). A directory without a binary is not an installation; it
// is the same "nothing to configure here" the absent case already
// reports, so it renders as a skip with the reason spelled out.
//
// binaryName is the executable looked for ("openclaw"); configDirLabel is
// how the directory is written in user-facing copy ("~/.openclaw").
func InstallationFinding(agent, binaryName, configDirLabel string, det Detection) AuditFinding {
	subject := agent + " installation"
	switch {
	case det.Found && det.BinaryPath != "":
		return AuditFinding{
			Status: StatusOK, Subject: subject,
			Detail: fmt.Sprintf("binary=%s configDir=%s", det.BinaryPath, det.ConfigDir),
		}
	case det.Found:
		return AuditFinding{
			Status: StatusSkip, Subject: subject,
			Detail: fmt.Sprintf("%s binary not on PATH; %s is present but is not an installation",
				binaryName, configDirLabel),
		}
	default:
		return AuditFinding{
			Status: StatusSkip, Subject: subject,
			Detail: fmt.Sprintf("%s binary not on PATH and %s is absent", binaryName, configDirLabel),
		}
	}
}

// ApplyOptions controls one Apply / Uninstall pass.
type ApplyOptions struct {
	// HomeDir is $HOME for the operation. Tests pass t.TempDir().
	HomeDir string
	// StateDir is the Waired state directory (typically
	// $XDG_CONFIG_HOME/waired or $HOME/.config/waired). The gateway
	// token and the integration ledger live under this tree.
	StateDir string
	// GatewayBaseURL is the base URL the agent should talk to (e.g.
	// "http://127.0.0.1:9473").
	GatewayBaseURL string
	// GatewayToken is the value to write into env vars and config
	// files. Always non-empty when Apply runs (the manager loads or
	// generates it before dispatching).
	GatewayToken string
	// Force makes the adapter ignore its Detect() result and apply
	// anyway (`waired link <agent>` semantics).
	Force bool
	// WiredBinary is the absolute path of the running waired binary.
	// Retained for adapters that may generate launchers; the claude-code
	// adapter no longer uses it (routing is via the transparent proxy).
	WiredBinary string
	// NonInteractive disables every prompt; the adapter must pick the
	// safe default. Non-interactive default for Apply == "go ahead".
	NonInteractive bool
	// Prompt is consulted for interactive y/n/manual decisions when
	// NonInteractive is false. Adapters MUST tolerate Prompt being nil
	// (treat as NonInteractive).
	Prompt Prompter
	// Logger receives structured progress updates.
	Logger Logger
}

// Prompter is the minimal interaction surface adapters need. Tests
// inject a scripted implementation; the CLI plugs in a tty-backed one.
type Prompter interface {
	// Confirm asks a Y/n/manual question. choice = "yes" | "no" |
	// "manual" (case insensitive). On no TTY the implementation
	// returns "yes" (Apply default) without blocking.
	Confirm(ctx context.Context, subject, message string) (choice string, err error)
}

// Logger is a 2-level structured logger. nil values are tolerated.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// nullLogger silently discards messages so adapters can call methods
// unconditionally without a nil check.
type nullLogger struct{}

func (nullLogger) Infof(string, ...any) {}
func (nullLogger) Warnf(string, ...any) {}

// EffectiveLogger returns l, or a no-op logger when l is nil.
func EffectiveLogger(l Logger) Logger {
	if l == nil {
		return nullLogger{}
	}
	return l
}

// Adapter is implemented once per supported coding agent.
type Adapter interface {
	// ID returns the canonical AgentID.
	ID() AgentID
	// Detect probes the host for installation evidence (binary on
	// $PATH, well-known config dir).
	Detect(ctx context.Context, opts ApplyOptions) (Detection, error)
	// Apply writes the agent-specific config / skills. Must be
	// idempotent: a second Apply over an already-applied install
	// MUST NOT prompt again, MUST NOT create duplicate sentinel
	// blocks, MUST refresh stale token / URL values.
	Apply(ctx context.Context, opts ApplyOptions) error
	// Audit reports the current state without mutating anything.
	Audit(ctx context.Context, opts ApplyOptions) ([]AuditFinding, error)
	// Uninstall removes everything Apply created. Best-effort: missing
	// files are not an error.
	Uninstall(ctx context.Context, opts ApplyOptions) error
}

// ErrAgentNotFound is returned by Manager methods when the requested
// agent is not registered.
var ErrAgentNotFound = errors.New("integration: agent not registered")

// AgentNotInstalledError is returned by Apply when an adapter cannot
// proceed because the agent is not detected and Force is false.
type AgentNotInstalledError struct {
	Agent AgentID
}

func (e *AgentNotInstalledError) Error() string {
	return fmt.Sprintf("integration: %s is not installed on this host", e.Agent)
}
