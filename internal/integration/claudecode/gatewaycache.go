package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// Claude Code /model picker cache (#332 / #407).
//
// Claude Code populates its /model picker from a gateway's GET /v1/models, but
// the fetch is gated: it is skipped unless ANTHROPIC_AUTH_TOKEN or a resolved
// API key (apiKeyHelper included) is configured. waired deliberately writes NO
// credential — that is what keeps the claude.ai subscription the active auth
// (#488) — so on a subscription-OAuth host discovery has never once run, the
// cache has never been written, and the three reserved directive ids have never
// appeared in the picker. Verified live: on such a host,
// ~/.claude/cache/gateway-models.json simply does not exist.
//
// The READ side has no such gate on the file's provenance: the picker falls
// back to whatever cache is already on disk (documented upstream as
// llm-gateway-protocol's fallback-to-cache). So waired writes it. This is the
// same thing every other credential-less, subscription-preserving gateway
// integration does.
//
// It is a private file of somebody else's client, so the shape below is
// measured, not assumed, and scripts/ci/canary-cache-schema.py re-measures it
// against each Claude Code release:
//
//   - fetchedAt is epoch MILLISECONDS, a JSON number. Not RFC3339. The reader
//     schema-parses the whole document, so a wrong type here does not degrade
//     gracefully — the parse yields null, the picker silently falls back to the
//     built-in list, and the result is indistinguishable from the bug this
//     exists to fix.
//   - baseUrl is compared by EXACT STRING against the live ANTHROPIC_BASE_URL.
//     A trailing slash disables the whole cache, silently.
//   - Only id and display_name survive per model; unknown fields are stripped
//     on read, so writing more is pointless and writing less is what we do.
//   - Mode 0600, and the parent cache/ directory is created if absent.
//
// A malformed file cannot break the client (the read is a memoized try/catch:
// the worst case is the empty picker of today), and routing is untouched by any
// of it — that lives in managed settings.

// gatewayCacheFile is the picker cache's filename. Pinned by the canary
// ("picker cache filename"): a rename upstream would make every write here a
// silent no-op, succeeding forever against a path nothing reads.
const gatewayCacheFile = "gateway-models.json"

// claudeConfigDirEnv relocates the whole Claude Code config tree, cache
// included. Pinned by the canary alongside the filename.
const claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

// GatewayCacheModel is one /model picker entry. The json tags ARE the contract
// — see the package comment on what the reader keeps.
type GatewayCacheModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

// gatewayCacheDoc is the on-disk document. FetchedAt is epoch milliseconds; the
// reader records it but applies no TTL, so it exists for shape compliance and
// for a human reading the file, not for expiry.
type gatewayCacheDoc struct {
	BaseURL   string              `json:"baseUrl"`
	FetchedAt int64               `json:"fetchedAt"`
	Models    []GatewayCacheModel `json:"models"`
}

// GatewayCacheDir returns the directory holding the picker cache: the
// CLAUDE_CONFIG_DIR tree when the invoking user has one, else ~/.claude.
//
// configDir is that env value as the CALLER read it, passed in rather than
// looked up here so the resolution is a pure function of (configDir, home) and
// can be table-tested across all three OSes from one host (CLAUDE.md
// §Cross-OS parity). home is only consulted when configDir is empty.
func GatewayCacheDir(configDir, home string) string {
	root := configDir
	if root == "" {
		root = filepath.Join(home, ".claude")
	}
	return filepath.Join(root, "cache")
}

// GatewayCachePath is GatewayCacheDir plus the filename.
func GatewayCachePath(configDir, home string) string {
	return filepath.Join(GatewayCacheDir(configDir, home), gatewayCacheFile)
}

// ClaudeConfigDir reads CLAUDE_CONFIG_DIR from the environment. Callers pass
// the result to GatewayCachePath; keeping the lookup here means the env var's
// name lives next to the code that depends on it, while the path logic itself
// stays pure.
func ClaudeConfigDir() string { return os.Getenv(claudeConfigDirEnv) }

// WriteGatewayCache writes the picker cache for the invoking user and returns
// the path written. baseURL must be the exact ANTHROPIC_BASE_URL string managed
// settings carry — the reader compares it byte for byte against the live value
// and ignores the file on any difference.
//
// now is injected so a test can assert the written timestamp rather than only
// its type; production callers pass time.Now.
func WriteGatewayCache(configDir, home, baseURL string, models []GatewayCacheModel, now func() time.Time) (string, error) {
	if baseURL == "" {
		return "", errors.New("claudecode: refusing to write the picker cache with an empty baseUrl")
	}
	if len(models) == 0 {
		// An empty list is a valid document that produces an empty picker —
		// exactly the symptom we are fixing. Refuse rather than ship it.
		return "", errors.New("claudecode: refusing to write an empty picker cache")
	}
	path := GatewayCachePath(configDir, home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("claudecode: mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.Marshal(gatewayCacheDoc{
		BaseURL:   baseURL,
		FetchedAt: now().UnixMilli(),
		Models:    models,
	})
	if err != nil {
		return "", fmt.Errorf("claudecode: marshal picker cache: %w", err)
	}
	// WriteSecret for the 0600 the client itself uses, and for its
	// temp-fsync-rename: a torn write would leave a document the reader parses
	// to null, i.e. the empty picker again. The parent directory is created
	// plainly rather than through secrets.SecureDir — cache/ belongs to Claude
	// Code, and re-ACLing somebody else's directory is not ours to do.
	if err := secrets.WriteSecret(path, append(data, '\n')); err != nil {
		return "", fmt.Errorf("claudecode: write %s: %w", path, err)
	}
	return path, nil
}

// GatewayCacheState is what a diagnostic surface can say about the picker
// cache without opening it itself.
//
// Present=false with no error means the file is simply absent, which is the
// ordinary "enable never ran for this user" case rather than a fault.
type GatewayCacheState struct {
	Path      string
	Present   bool
	BaseURL   string
	FetchedAt time.Time
	Models    []GatewayCacheModel
}

// ReadGatewayCache reads the picker cache the way Claude Code does, so a
// diagnostic can report what the CLIENT will make of it rather than what we
// hoped we wrote.
//
// Two failure modes are worth a surface of their own and are why this exists
// at all: an absent file (the picker shows Claude Code's built-ins only), and
// a baseUrl that does not byte-match the live ANTHROPIC_BASE_URL — a trailing
// slash or a changed port silently disables the whole cache with no error
// anywhere. Comparing the two is left to the caller, which is the one holding
// the live value.
//
// A malformed document is an error rather than "absent": the reader treats it
// as an empty picker, and reporting that as "not written yet" would send the
// operator to re-run enable when the file is right there.
func ReadGatewayCache(configDir, home string) (GatewayCacheState, error) {
	path := GatewayCachePath(configDir, home)
	st := GatewayCacheState{Path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("claudecode: read %s: %w", path, err)
	}
	var doc gatewayCacheDoc
	// waired writes this file itself and never adds a BOM, but it is in the
	// user's config directory and a person can edit it. Tolerating the mark
	// costs one call and keeps this reader consistent with the settings
	// readers (waired-agent#1067).
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &doc); err != nil {
		return st, fmt.Errorf("claudecode: parse %s: %w", path, err)
	}
	st.Present = true
	st.BaseURL = doc.BaseURL
	st.FetchedAt = time.UnixMilli(doc.FetchedAt)
	st.Models = doc.Models
	return st, nil
}

// RemoveGatewayCache deletes the picker cache, for `waired claude disable` and
// for the model-route-directives opt-out. Leaving it behind would keep offering
// ids that no longer route anywhere: the reader only checks that baseUrl
// matches the live base URL, so a stale file pointing at a gateway that is no
// longer configured is a documented way to get a picker full of dead entries.
// Absent is success.
func RemoveGatewayCache(configDir, home string) error {
	path := GatewayCachePath(configDir, home)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("claudecode: remove %s: %w", path, err)
	}
	return nil
}
