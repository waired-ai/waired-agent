package claudecode

// Taking away the picker cache a previous waired wrote (waired-agent#1185's
// upgrade path).
//
// Before #1185 the Waired rows reached the /model picker by waired writing
// Claude Code's own discovery cache, ~/.claude/cache/gateway-models.json, and
// setting CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY so the picker read it.
// The rows come from the `modelPicker` setting now, and the flag is scrubbed
// — but only by a root `waired claude enable`, and nothing removed the file
// at all.
//
// Measured on Claude Code 2.1.261 (2026-09-06): with the flag still set and
// the stale file still on disk, the picker shows BOTH — the three old rows
// under their old names and the new ones beside them. On a host that upgrades
// and does not immediately re-run enable as root, that is what the operator
// sees.
//
// So the per-user picker write takes the file away, which is the half that
// needs no elevation and runs on every `claude` launch through the
// SessionStart hook. The root half (scrubbing the flag) still happens at the
// next enable; by then there is nothing left for it to read.
//
// Ownership, as everywhere else: only a document that names THIS gateway and
// whose every row is a Waired id. A cache describing some other gateway is
// somebody else's — and Claude Code ignores it anyway, since it compares
// baseUrl against the live ANTHROPIC_BASE_URL by exact string.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// retiredCacheFile is the name Claude Code gives its discovery cache.
const retiredCacheFile = "gateway-models.json"

// claudeConfigDirEnv relocates the whole ~/.claude tree, this file included.
const claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

// ClaudeConfigDir reports CLAUDE_CONFIG_DIR, or "" when it is unset.
func ClaudeConfigDir() string { return os.Getenv(claudeConfigDirEnv) }

// RetiredCachePath is where the cache lives for this user.
func RetiredCachePath(configDir, home string) string {
	root := configDir
	if root == "" {
		root = filepath.Join(home, ".claude")
	}
	return filepath.Join(root, "cache", retiredCacheFile)
}

// retiredCacheDoc is the shape waired used to write. Only the two fields that
// decide ownership are decoded; anything else Claude Code has since added
// rides along and is irrelevant to the question.
type retiredCacheDoc struct {
	BaseURL string `json:"baseUrl"`
	Models  []struct {
		ID string `json:"id"`
	} `json:"models"`
}

// RemoveRetiredCache deletes the pre-#1185 picker cache when it is one waired
// wrote for this gateway, and reports whether it did.
//
// Absent, unreadable, unparseable, or describing a different gateway: left
// alone, no error. This runs inside a SessionStart hook on every launch, so a
// surprise here would be a failure on a path whose whole job is best-effort.
func RemoveRetiredCache(configDir, home, baseURL string) (removed bool, err error) {
	if baseURL == "" {
		return false, nil
	}
	path := RetiredCachePath(configDir, home)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, nil
	}
	var doc retiredCacheDoc
	if json.Unmarshal(b, &doc) != nil {
		return false, nil
	}
	if doc.BaseURL != baseURL || len(doc.Models) == 0 {
		return false, nil
	}
	for _, m := range doc.Models {
		if !IsWairedModelID(m.ID) {
			return false, nil
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}
