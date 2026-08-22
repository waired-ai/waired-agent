package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// LedgerVersion is the on-disk schema version. Bump only when an
// incompatible change to AgentRecord lands; readers should ignore
// fields they don't understand within the same major version.
const LedgerVersion = 1

// Ledger is the persisted record of every Apply mutation. Uninstall
// reads it to know exactly which sentinel blocks to remove and which
// files / config keys it owns.
//
// The ledger is rewritten whole on every Apply / Uninstall — not
// merged — so a successful Apply always produces a ledger that
// reflects the *current* on-disk state, not the union of every Apply
// that ever ran.
type Ledger struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	// Agents is keyed by AgentID.
	Agents map[AgentID]AgentRecord `json:"agents,omitempty"`
}

// AgentRecord captures every artefact a single Adapter installed.
// All fields are optional; an empty AgentRecord means "Apply ran but
// produced no per-agent files" (e.g. when only the shell-rc snippet
// was wanted).
type AgentRecord struct {
	// AppliedAt is the timestamp of the last successful Apply for
	// this agent.
	AppliedAt time.Time `json:"applied_at"`
	// SkillFiles is every file we created under the agent's skill /
	// command directory. Stored as absolute paths.
	SkillFiles []string `json:"skill_files,omitempty"`
	// SkillDirs is every directory we created (so Uninstall can
	// remove them when empty after files are deleted).
	SkillDirs []string `json:"skill_dirs,omitempty"`
	// ConfigPath is the third-party artifact we fully own or surgically
	// edited (e.g. the OpenCode plugin ~/.config/opencode/plugin/waired.js).
	// Empty when not applicable.
	ConfigPath string `json:"config_path,omitempty"`
	// OwnedFully is true when Apply created the config file from
	// scratch — Uninstall may remove it entirely.
	OwnedFully bool `json:"owned_fully,omitempty"`
	// AddedPaths lists JSON-pointer-ish dotted paths Apply inserted
	// into ConfigPath when OwnedFully=false. Uninstall deletes only
	// these. Example: ["provider.waired", "model"].
	AddedPaths []string `json:"added_paths,omitempty"`
	// BackupPath is the .waired-bak-<unix-ts> sidecar Apply created
	// before mutating an existing third-party config. Empty when
	// no backup was taken.
	BackupPath string `json:"backup_path,omitempty"`
	// VSCodeConfigs records each VSCode-family settings.json we
	// surgically edited (one entry per detected variant: Code,
	// Insiders, VSCodium, Cursor). Distinct from the single ConfigPath
	// above because a host can have several editors installed at once.
	// Uninstall reverts each entry independently. Empty when the
	// claude-code adapter did not configure any IDE.
	VSCodeConfigs []ManagedJSONConfig `json:"vscode_configs,omitempty"`
}

// ManagedJSONConfig captures one third-party JSON/JSONC file Apply
// surgically edited (e.g. a VSCode User settings.json). It records just
// enough for a precise, comment-preserving Uninstall: which keys we
// inserted and where the pre-edit backup lives.
type ManagedJSONConfig struct {
	// Path is the absolute path of the edited file.
	Path string `json:"path"`
	// BackupPath is the .waired-bak-<unix-ts> sidecar taken before the
	// first mutation. Empty when the file did not exist beforehand
	// (nothing to back up — Uninstall removes the keys / file outright).
	BackupPath string `json:"backup_path,omitempty"`
	// AddedKeys lists the top-level keys Apply inserted. Uninstall
	// removes only these (and only when their value is still ours), so
	// a key the user later re-pointed is left untouched. Example:
	// ["claudeCode.claudeProcessWrapper"].
	AddedKeys []string `json:"added_keys,omitempty"`
}

// LoadLedger reads the ledger at path. Returns a zero-value Ledger
// (Version 0, no agents) when the file does not exist.
func LoadLedger(path string) (*Ledger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Ledger{Version: LedgerVersion, Agents: map[AgentID]AgentRecord{}}, nil
		}
		return nil, fmt.Errorf("integration: read ledger %s: %w", path, err)
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("integration: parse ledger %s: %w", path, err)
	}
	if l.Agents == nil {
		l.Agents = map[AgentID]AgentRecord{}
	}
	return &l, nil
}

// Save writes the ledger atomically (write-then-rename, mode 0644).
// Sets UpdatedAt and Version automatically.
func (l *Ledger) Save(path string) error {
	if l.Agents == nil {
		l.Agents = map[AgentID]AgentRecord{}
	}
	l.Version = LedgerVersion
	l.UpdatedAt = time.Now().UTC()
	body, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("integration: marshal ledger: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("integration: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("integration: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Get returns the record for agent. The second return is false when
// no record exists; callers may still use the zero AgentRecord to mean
// "nothing to undo".
func (l *Ledger) Get(agent AgentID) (AgentRecord, bool) {
	r, ok := l.Agents[agent]
	return r, ok
}

// Set replaces the record for agent.
func (l *Ledger) Set(agent AgentID, r AgentRecord) {
	if l.Agents == nil {
		l.Agents = map[AgentID]AgentRecord{}
	}
	l.Agents[agent] = r
}

// Delete removes the record for agent. No-op when absent.
func (l *Ledger) Delete(agent AgentID) {
	delete(l.Agents, agent)
}
