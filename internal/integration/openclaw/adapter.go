// Package openclaw implements the Adapter for OpenClaw
// (openclaw/openclaw — a Node/TypeScript AI assistant CLI).
//
// OpenClaw is wired through a self-contained, waired-authored plugin under
// ~/.openclaw/plugins/waired/ (see plugin.go) plus a small surgical merge
// into ~/.openclaw/openclaw.json (see openclawjson.go). The plugin
// registers an independent "waired" provider whose resolveDynamicModel maps
// waired/{auto,coding,small} to the agent's no-token loopback data-plane
// gateway, and whose resolveSyntheticAuth supplies a non-secret local marker
// so no API key or environment variable is required.
//
// Why a plugin AND a config edit: OpenClaw does not auto-scan
// ~/.openclaw/plugins, and config-origin plugins are disabled by default, so
// the plugin must be registered (plugins.load.paths) and enabled
// (plugins.entries.waired.enabled). The model picker only surfaces
// allowlisted refs, so the three waired models are added to
// agents.defaults.models. The user's default model (agents.defaults.model)
// is never touched. A plugin alone cannot make a brand-new provider's models
// resolvable for inference — the resolveDynamicModel provider hook is what
// does, and it also controls the on-wire model string. See docs/decisions.md
// and the work record for the spike that established this.
package openclaw

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// New returns the OpenClaw adapter.
func New() integration.Adapter { return &adapter{} }

type adapter struct{}

func (a *adapter) ID() integration.AgentID { return integration.AgentOpenClaw }

func (a *adapter) Detect(_ context.Context, opts integration.ApplyOptions) (integration.Detection, error) {
	det := integration.Detection{}
	if path, ok := integration.LookPath("openclaw"); ok {
		det.Found = true
		det.BinaryPath = path
		det.Notes = append(det.Notes, fmt.Sprintf("openclaw on PATH: %s", path))
	}
	// Key the config-dir signal on content waired did NOT write. Apply
	// MkdirAll's ~/.openclaw itself plus a plugins/ tree and merges its own
	// keys into openclaw.json, so a bare DirExists check self-poisons once
	// applied (waired#753). configDirLooksInstalled ignores exactly that
	// waired footprint (see openclawjson.go).
	configDir := ConfigDir(opts.HomeDir)
	if configDirLooksInstalled(opts.HomeDir) {
		det.Found = true
		det.ConfigDir = configDir
		det.Notes = append(det.Notes, fmt.Sprintf("~/.openclaw has non-waired content: %s", configDir))
	}
	return det, nil
}

// Apply writes the waired OpenClaw plugin and merges the owned keys into
// openclaw.json. Idempotent: the plugin is rewritten and the config keys are
// upserted, with a one-shot backup of an existing config before mutation.
func (a *adapter) Apply(ctx context.Context, opts integration.ApplyOptions) error {
	if opts.HomeDir == "" {
		return fmt.Errorf("openclaw: empty HomeDir")
	}
	if opts.GatewayBaseURL == "" {
		return fmt.Errorf("openclaw: empty GatewayBaseURL")
	}

	if !opts.Force {
		det, err := a.Detect(ctx, opts)
		if err != nil {
			return err
		}
		if !det.Found {
			return &integration.AgentNotInstalledError{Agent: a.ID()}
		}
	}

	logger := integration.EffectiveLogger(opts.Logger)

	pluginFiles, err := installPlugin(opts.HomeDir, opts.GatewayBaseURL)
	if err != nil {
		return err
	}
	logger.Infof("openclaw: wrote plugin %s (provider 'waired' -> %s)", PluginDir(opts.HomeDir), providerBaseURL(opts.GatewayBaseURL))

	configPath := ConfigFile(opts.HomeDir)
	m, existed, err := readConfigObject(configPath)
	if err != nil {
		return err
	}
	var backupPath string
	if existed {
		if backupPath, err = backupConfig(configPath); err != nil {
			return err
		}
	}
	if err := mergeConfig(m, PluginDir(opts.HomeDir)); err != nil {
		return err
	}
	body, err := marshalConfig(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(opts.HomeDir), 0o755); err != nil {
		return fmt.Errorf("openclaw: mkdir %s: %w", ConfigDir(opts.HomeDir), err)
	}
	if err := writeFileAtomic(configPath, body, 0o644); err != nil {
		return err
	}
	logger.Infof("openclaw: registered+enabled plugin in %s (models %v allowlisted)", configPath, modelRefs())

	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		return err
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		return err
	}
	ledger.Set(a.ID(), integration.AgentRecord{
		AppliedAt:  time.Now().UTC(),
		SkillFiles: pluginFiles,
		SkillDirs:  []string{PluginDir(opts.HomeDir)},
		ConfigPath: configPath,
		OwnedFully: !existed,
		AddedPaths: managedAddedPaths(),
		BackupPath: backupPath,
	})
	return ledger.Save(paths.Ledger)
}

func (a *adapter) Audit(_ context.Context, opts integration.ApplyOptions) ([]integration.AuditFinding, error) {
	var out []integration.AuditFinding

	// Plugin entry file: present + points at the expected data-plane URL.
	entryFile := PluginEntryFile(opts.HomeDir)
	body, err := os.ReadFile(entryFile)
	switch {
	case err != nil && os.IsNotExist(err):
		out = append(out, integration.AuditFinding{
			Status: integration.StatusFail, Subject: "openclaw plugin",
			Detail: fmt.Sprintf("missing: %s", entryFile),
		})
	case err != nil:
		out = append(out, integration.AuditFinding{
			Status: integration.StatusFail, Subject: "openclaw plugin", Detail: err.Error(),
		})
	default:
		wantURL := providerBaseURL(opts.GatewayBaseURL)
		switch {
		case strings.Contains(string(body), "registerProvider") && strings.Contains(string(body), wantURL):
			out = append(out, integration.AuditFinding{
				Status: integration.StatusOK, Subject: "openclaw plugin", Detail: entryFile,
			})
		case strings.Contains(string(body), "registerProvider"):
			out = append(out, integration.AuditFinding{
				Status: integration.StatusWarn, Subject: "openclaw plugin",
				Detail: fmt.Sprintf("baseURL drifted from %s in %s", wantURL, entryFile),
			})
		default:
			out = append(out, integration.AuditFinding{
				Status: integration.StatusFail, Subject: "openclaw plugin",
				Detail: fmt.Sprintf("registerProvider not found in %s", entryFile),
			})
		}
	}

	// openclaw.json: plugin registered + enabled.
	out = append(out, auditConfig(opts.HomeDir))

	det, err := a.Detect(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	if det.Found {
		out = append(out, integration.AuditFinding{
			Status: integration.StatusOK, Subject: "openclaw installation",
			Detail: fmt.Sprintf("binary=%s configDir=%s", det.BinaryPath, det.ConfigDir),
		})
	} else {
		out = append(out, integration.AuditFinding{
			Status: integration.StatusSkip, Subject: "openclaw installation",
			Detail: "openclaw binary not on PATH and ~/.openclaw is absent",
		})
	}
	return out, nil
}

// auditConfig checks that openclaw.json registers the plugin dir in
// plugins.load.paths and enables it in plugins.entries.waired.
func auditConfig(home string) integration.AuditFinding {
	configPath := ConfigFile(home)
	m, existed, err := readConfigObject(configPath)
	if err != nil {
		return integration.AuditFinding{Status: integration.StatusFail, Subject: "openclaw config", Detail: err.Error()}
	}
	if !existed {
		return integration.AuditFinding{Status: integration.StatusFail, Subject: "openclaw config", Detail: fmt.Sprintf("missing: %s", configPath)}
	}
	pluginDir := PluginDir(home)
	registered := false
	if plugins := childMapNoCreate(m, "plugins"); plugins != nil {
		if load := childMapNoCreate(plugins, "load"); load != nil {
			registered = containsString(stringSlice(load["paths"]), pluginDir)
		}
		if entries := childMapNoCreate(plugins, "entries"); entries != nil {
			if w := childMapNoCreate(entries, "waired"); w != nil {
				if en, ok := w["enabled"].(bool); ok && en && registered {
					return integration.AuditFinding{Status: integration.StatusOK, Subject: "openclaw config", Detail: configPath}
				}
			}
		}
	}
	return integration.AuditFinding{
		Status: integration.StatusWarn, Subject: "openclaw config",
		Detail: fmt.Sprintf("plugin not registered+enabled in %s (run `waired link openclaw`)", configPath),
	}
}

func (a *adapter) Uninstall(_ context.Context, opts integration.ApplyOptions) error {
	paths, err := integration.PathsFor(opts.StateDir)
	if err != nil {
		return err
	}
	ledger, err := integration.LoadLedger(paths.Ledger)
	if err != nil {
		return err
	}

	if err := removePlugin(opts.HomeDir); err != nil {
		return err
	}

	configPath := ConfigFile(opts.HomeDir)
	m, existed, err := readConfigObject(configPath)
	if err != nil {
		return err
	}
	if existed {
		removeManagedKeys(m, PluginDir(opts.HomeDir))
		if isEffectivelyEmpty(m) {
			if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("openclaw: remove %s: %w", configPath, err)
			}
		} else {
			body, err := marshalConfig(m)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(configPath, body, 0o644); err != nil {
				return err
			}
		}
	}

	ledger.Delete(a.ID())
	return ledger.Save(paths.Ledger)
}
