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

// defaultGatewayBaseURL is where the plugin points when the caller
// hands over something unusable. It MUST match
// agentconfig.Defaults().Inference.LocalGatewayPort (9473).
const defaultGatewayBaseURL = "http://127.0.0.1:9473"

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

// renderPlugin produces the plugin JS for the given gateway base URL.
// Exposed for tests.
func renderPlugin(gatewayBaseURL string) ([]byte, error) {
	tmpl, err := template.ParseFS(pluginTemplate, "templates/plugin_waired.js.tmpl")
	if err != nil {
		return nil, fmt.Errorf("opencode: parse plugin template: %w", err)
	}
	// JSON-encode the URL so it is a safe JS string literal.
	baseLit, err := json.Marshal(GatewayBaseURL(gatewayBaseURL) + "/v1")
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
// installPlugin renders the waired OpenCode provider plugin into
// PluginDir(home)/waired.js and returns the file path. Idempotent: an
// existing file is overwritten via tmp+rename.
func installPlugin(home, gatewayBaseURL string) (string, error) {
	dir := PluginDir(home)
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
