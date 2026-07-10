package openclaw

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

//go:embed templates/index.mjs.tmpl templates/openclaw.plugin.json templates/package.json
var pluginTemplates embed.FS

// defaultDataPlanePort is the loopback port of the agent's no-token
// data-plane gateway, shared with the OpenCode integration. It MUST match
// agentconfig.Defaults().Inference.OpenCodeGatewayPort (9479). The plugin
// points the provider baseURL here rather than at the main (token-gated)
// gateway, because the desktop user cannot read the agent's 0600 token in
// the system-service deployment.
const defaultDataPlanePort = "9479"

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

// DataPlaneBaseURL derives the OpenClaw no-token data-plane base URL from
// the main gateway base URL by swapping the port to the data-plane port,
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

// providerBaseURL is the OpenAI-compatible base URL the plugin's models
// target (the data-plane URL with the /v1 suffix the transport expects).
func providerBaseURL(gatewayBaseURL string) string {
	return DataPlaneBaseURL(gatewayBaseURL) + "/v1"
}

// renderEntry produces the plugin index.mjs for the given gateway base URL.
// Exposed for tests.
func renderEntry(gatewayBaseURL string) ([]byte, error) {
	tmpl, err := template.ParseFS(pluginTemplates, "templates/index.mjs.tmpl")
	if err != nil {
		return nil, fmt.Errorf("openclaw: parse plugin template: %w", err)
	}
	// JSON-encode the URL so it is a safe JS string literal.
	baseLit, err := json.Marshal(providerBaseURL(gatewayBaseURL))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"BaseURLLiteral": string(baseLit)}); err != nil {
		return nil, fmt.Errorf("openclaw: render plugin: %w", err)
	}
	return buf.Bytes(), nil
}

// installPlugin renders + writes the three plugin files into
// <home>/.openclaw/plugins/waired/. Returns the file paths (for the
// ledger). Idempotent: existing files are overwritten via tmp+rename.
func installPlugin(home, gatewayBaseURL string) ([]string, error) {
	dir := PluginDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("openclaw: mkdir %s: %w", dir, err)
	}

	entry, err := renderEntry(gatewayBaseURL)
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
