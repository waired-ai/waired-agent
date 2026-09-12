package openclaw

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
)

//go:embed templates/index.mjs.tmpl templates/openclaw.plugin.json templates/package.json
var pluginTemplates embed.FS

// defaultGatewayBaseURL is where the plugin points when the caller
// hands over something unusable. It MUST match
// agentconfig.Defaults().Inference.LocalGatewayPort (9473).
const defaultGatewayBaseURL = "http://127.0.0.1:9473"

// pluginFileNames are the three files that make up the waired OpenClaw
// plugin directory. index.mjs is rendered from a template; the other two
// are copied verbatim.
var pluginFileNames = []string{"package.json", "openclaw.plugin.json", "index.mjs"}

// PluginManifestFile / PluginEntryFile / PluginPackageFile return the
// on-disk paths of the three plugin files under PluginDir(home).
func PluginManifestFile(home string) string {
	return filepath.Join(PluginDir(home), "openclaw.plugin.json")
}
func PluginEntryFile(home string) string   { return filepath.Join(PluginDir(home), "index.mjs") }
func PluginPackageFile(home string) string { return filepath.Join(PluginDir(home), "package.json") }

// GatewayBaseURL normalises the base URL the plugin's provider points at.
// It used to swap the port to a second, token-less listener on 9479,
// because the desktop user could not read the 0600 bearer token the main
// gateway required. There is no token and no second listener any more
// (waired-ai/waired#1277), so the gateway URL the caller resolved — from
// agent.json, so a pinned port reaches here — is used as given. A
// malformed or empty input falls back to the loopback default.
func GatewayBaseURL(gatewayBaseURL string) string {
	u, err := url.Parse(gatewayBaseURL)
	if err != nil || u.Host == "" {
		return defaultGatewayBaseURL
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}

// providerBaseURL is the OpenAI-compatible base URL the plugin's models
// target (the data-plane URL with the /v1 suffix the transport expects).
func providerBaseURL(gatewayBaseURL string) string {
	return GatewayBaseURL(gatewayBaseURL) + "/v1"
}

// pluginRow is one row as the plugin file carries it. The key is the wire id
// minus its "waired/" head, because that is what OpenClaw hands
// resolveDynamicModel and what the picker composes its reference from.
type pluginRow struct {
	Key           string `json:"key"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
}

// pluginRows projects the gateway's route rows into what the template writes.
//
// An empty answer renders the one row that needs no facts about the mesh, so a
// plugin written before anything is serving is the plugin this integration
// shipped before waired-agent#1306 rather than an empty picker.
func pluginRows(rows []modelrows.Row) []pluginRow {
	out := make([]pluginRow, 0, len(rows))
	for _, r := range rows {
		key := strings.TrimPrefix(r.ID, "waired/")
		if key == "" || key == r.ID {
			// Not a "waired/<key>" id. The bare any-node spelling is one, and
			// it cannot be addressed here: OpenClaw reads a model reference as
			// <provider>/<model>, so a key has to be the second segment.
			continue
		}
		out = append(out, pluginRow{Key: key, Name: r.DisplayName, ContextWindow: r.ContextWindow})
	}
	if len(out) == 0 {
		out = append(out, pluginRow{Key: defaultModelKey, Name: "Waired Default"})
	}
	return out
}

// modelRefs is the set of picker references the adapter allowlists in
// agents.defaults.models, derived from the same rows the plugin carries.
func modelRefs(rows []pluginRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelRefPrefix+r.Key)
	}
	return out
}

// renderEntry produces the plugin index.mjs for the given gateway base URL,
// context window and rows. A window of 0 means "not known" and renders a
// plugin that declares no contextWindow of its own. Exposed for tests.
func renderEntry(gatewayBaseURL string, contextWindow int, rows []pluginRow) ([]byte, error) {
	tmpl, err := template.ParseFS(pluginTemplates, "templates/index.mjs.tmpl")
	if err != nil {
		return nil, fmt.Errorf("openclaw: parse plugin template: %w", err)
	}
	// JSON-encode the URL so it is a safe JS string literal.
	baseLit, err := json.Marshal(providerBaseURL(gatewayBaseURL))
	if err != nil {
		return nil, err
	}
	if contextWindow < 0 {
		contextWindow = 0
	}
	if len(rows) == 0 {
		rows = pluginRows(nil)
	}
	// JSON is a subset of JS object syntax, so the marshalled rows are a
	// literal the plugin can read as written.
	rowsLit, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	data := map[string]string{
		"BaseURLLiteral":       string(baseLit),
		"ContextWindowLiteral": strconv.Itoa(contextWindow),
		"ModelsLiteral":        string(rowsLit),
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("openclaw: render plugin: %w", err)
	}
	return buf.Bytes(), nil
}

// installPlugin renders + writes the three plugin files into
// <home>/.openclaw/plugins/waired/. Returns the file paths (for the
// ledger). Idempotent: existing files are overwritten via tmp+rename.
func installPlugin(home, gatewayBaseURL string, contextWindow int, rows []pluginRow) ([]string, error) {
	dir := PluginDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("openclaw: mkdir %s: %w", dir, err)
	}

	entry, err := renderEntry(gatewayBaseURL, contextWindow, rows)
	if err != nil {
		return nil, err
	}
	manifest, err := pluginTemplates.ReadFile("templates/openclaw.plugin.json")
	if err != nil {
		return nil, fmt.Errorf("openclaw: read manifest template: %w", err)
	}
	pkg, err := pluginTemplates.ReadFile("templates/package.json")
	if err != nil {
		return nil, fmt.Errorf("openclaw: read package template: %w", err)
	}

	bodies := map[string][]byte{
		"package.json":         pkg,
		"openclaw.plugin.json": manifest,
		"index.mjs":            entry,
	}
	var written []string
	for _, name := range pluginFileNames {
		dst := filepath.Join(dir, name)
		if err := writeFileAtomic(dst, bodies[name], 0o644); err != nil {
			return nil, err
		}
		written = append(written, dst)
	}
	return written, nil
}

// removePlugin deletes the three plugin files (best-effort) and removes
// the plugin directory and its parent plugins/ directory only when each is
// left empty (user-added plugins stay put).
func removePlugin(home string) error {
	dir := PluginDir(home)
	for _, name := range pluginFileNames {
		dst := filepath.Join(dir, name)
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("openclaw: remove %s: %w", dst, err)
		}
	}
	removeDirIfEmpty(dir)
	removeDirIfEmpty(filepath.Dir(dir)) // ~/.openclaw/plugins
	return nil
}

// removeDirIfEmpty removes dir only when it contains no entries.
func removeDirIfEmpty(dir string) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

// writeFileAtomic writes data to path via tmp+rename so a crashed write
// never leaves a half-written file behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("openclaw: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("openclaw: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
