// Package opencode implements the Adapter for OpenCode.
//
// OpenCode is wired through a single waired-authored plugin file at
// ~/.config/opencode/plugin/waired.js (see plugin.go). The plugin uses
// OpenCode's `config` hook to register an independent "waired" provider
// pointing at the agent's no-token loopback data-plane gateway.
//
// This replaces the earlier approach of surgically editing
// ~/.config/opencode/opencode.json: OpenCode only surfaces a provider that
// has a config-side stanza (a catalog entry alone yields "Provider not
// found"), and a self-contained plugin file we fully own is cleaner to
// install and remove than merging into the user's opencode.json — no
// backup, no surgical key removal, one-file uninstall. See
// docs/decisions.md and the work record for the spike that established this.
//
// Two affordance command files (waired-status, waired-doctor) are also
// installed under ~/.config/opencode/commands/.
package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// New returns the OpenCode adapter.
func New() integration.Adapter { return &adapter{} }

type adapter struct{}

func (a *adapter) ID() integration.AgentID { return integration.AgentOpenCode }

func (a *adapter) Detect(_ context.Context, opts integration.ApplyOptions) (integration.Detection, error) {
	det := integration.Detection{}
	if path, ok := integration.LookPath("opencode"); ok {
		det.Found = true
		det.BinaryPath = path
		det.Notes = append(det.Notes, fmt.Sprintf("opencode on PATH: %s", path))
	}
	// Key the config-dir signal on content waired did NOT write. A plain
	// DirExists check self-poisons: Apply MkdirAll's plugin/ and commands/
	// under this dir, so once applied the dir always exists (waired#753).
	// The user's own opencode.json / auth.json (waired never writes them)
	// is the foreign entry that marks a real install.
	configDir := ConfigDir(opts.HomeDir)
	if integration.ConfigDirHasForeignEntry(configDir,
		filepath.Base(PluginDir(opts.HomeDir)),
		filepath.Base(CommandsDir(opts.HomeDir))) {
		det.Found = true
		det.ConfigDir = configDir
		det.Notes = append(det.Notes, fmt.Sprintf("~/.config/opencode has non-waired content: %s", configDir))
	}
	return det, nil
}

// Apply writes the waired OpenCode plugin and the affordance command
// files. Idempotent: the plugin is rewritten via tmp+rename and the
// command files are overwritten.
func (a *adapter) Apply(ctx context.Context, opts integration.ApplyOptions) error {
	if opts.HomeDir == "" {
		return fmt.Errorf("opencode: empty HomeDir")
	}
	if opts.GatewayBaseURL == "" {
		return fmt.Errorf("opencode: empty GatewayBaseURL")
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

	pluginFile, err := installPlugin(opts.HomeDir, opts.GatewayBaseURL)
	if err != nil {
		return err
	}
	logger.Infof("opencode: wrote plugin %s (provider 'waired' -> %s)", pluginFile, DataPlaneBaseURL(opts.GatewayBaseURL)+"/v1")

	cmdFiles, err := installCommands(opts.HomeDir)
	if err != nil {
		return err
	}

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
		ConfigPath: pluginFile, // the plugin file is the primary artifact
		OwnedFully: true,       // we fully own it; Uninstall deletes it outright
		SkillFiles: cmdFiles,
		SkillDirs:  []string{CommandsDir(opts.HomeDir), PluginDir(opts.HomeDir)},
	})
	return ledger.Save(paths.Ledger)
}

func (a *adapter) Audit(_ context.Context, opts integration.ApplyOptions) ([]integration.AuditFinding, error) {
	var out []integration.AuditFinding

	pluginFile := PluginFile(opts.HomeDir)
	body, err := os.ReadFile(pluginFile)
	switch {
	case err != nil && os.IsNotExist(err):
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "opencode plugin",
			Detail:  fmt.Sprintf("missing: %s", pluginFile),
		})
	case err != nil:
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "opencode plugin",
			Detail:  err.Error(),
		})
	default:
		wantURL := DataPlaneBaseURL(opts.GatewayBaseURL) + "/v1"
		switch {
		case strings.Contains(string(body), "provider.waired") && strings.Contains(string(body), wantURL):
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusOK,
				Subject: "opencode plugin",
				Detail:  pluginFile,
			})
		case strings.Contains(string(body), "provider.waired"):
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusWarn,
				Subject: "opencode plugin",
				Detail:  fmt.Sprintf("baseURL drifted from %s in %s", wantURL, pluginFile),
			})
		default:
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusFail,
				Subject: "opencode plugin",
				Detail:  fmt.Sprintf("provider.waired not found in %s", pluginFile),
			})
		}
	}

	for _, c := range installedCommands() {
		path := CommandFile(opts.HomeDir, c.Name)
		fi, statErr := os.Stat(path)
		switch {
		case statErr == nil && fi.Mode().IsRegular():
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusOK,
				Subject: fmt.Sprintf("opencode command %s", c.Name),
				Detail:  path,
			})
		case os.IsNotExist(statErr):
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusFail,
				Subject: fmt.Sprintf("opencode command %s", c.Name),
				Detail:  fmt.Sprintf("missing: %s", path),
			})
		default:
			out = append(out, integration.AuditFinding{
				Status:  integration.StatusFail,
				Subject: fmt.Sprintf("opencode command %s", c.Name),
				Detail:  fmt.Sprintf("stat %s: %v", path, statErr),
			})
		}
	}

	det, err := a.Detect(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	if det.Found {
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusOK,
			Subject: "opencode installation",
			Detail:  fmt.Sprintf("binary=%s configDir=%s", det.BinaryPath, det.ConfigDir),
		})
	} else {
		out = append(out, integration.AuditFinding{
			Status:  integration.StatusSkip,
			Subject: "opencode installation",
			Detail:  "opencode binary not on PATH and ~/.config/opencode is absent",
		})
	}
	return out, nil
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
	rec, _ := ledger.Get(a.ID())

	if err := removePlugin(opts.HomeDir); err != nil {
		return err
	}

	cmdFiles := rec.SkillFiles
	if len(cmdFiles) == 0 {
		// Best-effort fallback: synthesize canonical names.
		for _, c := range installedCommands() {
			cmdFiles = append(cmdFiles, CommandFile(opts.HomeDir, c.Name))
		}
	}
	if err := removeCommands(cmdFiles, opts.HomeDir); err != nil {
		return err
	}

	ledger.Delete(a.ID())
	return ledger.Save(paths.Ledger)
}
