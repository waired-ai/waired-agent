package opencode

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed templates/plugin_waired.js.tmpl
var pluginTemplate embed.FS

// defaultDataPlanePort is the loopback port of the agent's no-token
// OpenCode data-plane gateway. It MUST match
// agentconfig.Defaults().Inference.OpenCodeGatewayPort (9479). The plugin
// points the provider baseURL here rather than at the main (token-gated)
// gateway, because the desktop user cannot read the agent's 0600 token in
// the system-service deployment.
const defaultDataPlanePort = "9479"

// PluginDir returns the OpenCode global plugin directory
// (~/.config/opencode/plugin). OpenCode loads every *.js/*.ts file there
// at startup.
func PluginDir(home string) string {
	return filepath.Join(ConfigDir(home), "plugin")
}

// PluginFile returns the on-disk path of the waired plugin.
func PluginFile(home string) string {
	return filepath.Join(PluginDir(home), "waired.js")
}

// DataPlaneBaseURL derives the OpenCode no-token data-plane base URL from
// the main gateway base URL by swapping the port to the OpenCode port,
// e.g. "http://127.0.0.1:9473" -> "http://127.0.0.1:9479". A malformed or
// empty input falls back to the loopback default. (A non-default
// OpenCodeGatewayPort is not threaded here yet; see the work record.)
func DataPlaneBaseURL(gatewayBaseURL string) string {
	u, err := url.Parse(gatewayBaseURL)
	if err != nil || u.Host == "" {
		return "http://127.0.0.1:" + defaultDataPlanePort
	}
	host := u.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + host + ":" + defaultDataPlanePort
}

// renderPlugin produces the plugin JS for the given gateway base URL.
// Exposed for tests.
func renderPlugin(gatewayBaseURL string) ([]byte, error) {
	tmpl, err := template.ParseFS(pluginTemplate, "templates/plugin_waired.js.tmpl")
	if err != nil {
		return nil, fmt.Errorf("opencode: parse plugin template: %w", err)
	}
	// JSON-encode the URL so it is a safe JS string literal.
	baseLit, err := json.Marshal(DataPlaneBaseURL(gatewayBaseURL) + "/v1")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"BaseURLLiteral": string(baseLit)}); err != nil {
		return nil, fmt.Errorf("opencode: render plugin: %w", err)
	}
	return buf.Bytes(), nil
}

// installPlugin renders the waired OpenCode plugin into
// <home>/.config/opencode/plugin/waired.js. Returns the file path for the
// ledger. Idempotent: an existing file is overwritten via tmp+rename.
func installPlugin(home, gatewayBaseURL string) (string, error) {
	return WritePluginInConfigDir(ConfigDir(home), gatewayBaseURL)
}

// WritePluginInConfigDir renders the waired OpenCode provider plugin into
// <configDir>/plugin/waired.js and returns the file path. It is the
// config-dir-explicit form of installPlugin, used by the bundled
// coding-agent runtime (internal/runtime/codeui), which drives an
// OpenCode instance with OPENCODE_CONFIG_DIR pointed at a waired-owned
// isolated directory rather than the user's ~/.config/opencode.
// Idempotent: an existing file is overwritten via tmp+rename.
func WritePluginInConfigDir(configDir, gatewayBaseURL string) (string, error) {
	dir := filepath.Join(configDir, "plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("opencode: mkdir %s: %w", dir, err)
	}
	body, err := renderPlugin(gatewayBaseURL)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "waired.js")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return "", fmt.Errorf("opencode: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("opencode: rename %s -> %s: %w", tmp, dst, err)
	}
	return dst, nil
}

// removePlugin deletes the waired plugin file (best-effort) and removes
// the plugin/ directory only when it is left empty (user-added plugins
// stay put).
func removePlugin(home string) error {
	dst := PluginFile(home)
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("opencode: remove %s: %w", dst, err)
	}
	dir := PluginDir(home)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
	return nil
}
