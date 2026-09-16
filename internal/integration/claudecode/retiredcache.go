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
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// retiredCacheLoopbackPrefix is the start of every base URL waired has
// written into managed settings, and so of every baseUrl a cache waired
// wrote can carry. The same signature claudemanaged recognises its own
// ANTHROPIC_BASE_URL by.
const retiredCacheLoopbackPrefix = "http://127.0.0.1:"

// RemoveRetiredCacheOwned is RemoveRetiredCache for `waired claude disable`
// and the uninstallers, where there is no live base URL to compare against:
// disable removes the managed ANTHROPIC_BASE_URL before it reaches the
// per-user cleanup, and a host whose binary is already gone never had one to
// hand over. Asking for an exact match there meant the cache was never removed
// at all (waired-agent#1398).
//
// So ownership is recognised by the document alone: a loopback baseUrl and a
// non-empty model list in which every id is a Waired id. A cache naming any
// other gateway, or listing any other model, is left alone. A leading UTF-8
// BOM is tolerated, as it is for the settings files.
//
// packaging/install/uninstall.ps1 and uninstall.sh carry the same rule for a
// host with no binary; packaging/install/testdata/claude-leftovers holds the
// cases all of them are held to.
func RemoveRetiredCacheOwned(configDir, home string) (removed bool, err error) {
	path := RetiredCachePath(configDir, home)
	b, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var doc retiredCacheDoc
	if json.Unmarshal(bytes.TrimPrefix(b, utf8BOM), &doc) != nil {
		return false, nil
	}
	if !strings.HasPrefix(doc.BaseURL, retiredCacheLoopbackPrefix) || len(doc.Models) == 0 {
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

// fallbackMarkerDir is where the retired Stop hook kept one small file per
// Claude Code session, under the user's cache directory
// (os.UserCacheDir()/waired/claude-fallback). The hook and its writer went in
// #1184 and nothing removed the directory after them (waired-agent#1398).
const fallbackMarkerDir = "claude-fallback"

// RemoveRetiredFallbackMarkers deletes <cacheDir>/waired/claude-fallback, and
// <cacheDir>/waired with it when that leaves it empty. cacheDir is the
// invoking user's os.UserCacheDir(). Nothing under that directory was ever
// anyone's but waired's, so there is no ownership question to ask of its
// contents. An empty cacheDir or a missing directory reports false.
func RemoveRetiredFallbackMarkers(cacheDir string) (removed bool, err error) {
	if cacheDir == "" {
		return false, nil
	}
	parent := filepath.Join(cacheDir, "waired")
	dir := filepath.Join(parent, fallbackMarkerDir)
	if _, err := os.Lstat(dir); err != nil {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
		_ = os.Remove(parent)
	}
	return true, nil
}
