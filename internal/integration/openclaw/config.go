package openclaw

import "path/filepath"

// ConfigDir returns the OpenClaw config / state directory under home
// (~/.openclaw). This is where openclaw.json and the plugins tree live.
func ConfigDir(home string) string {
	return filepath.Join(home, ".openclaw")
}

// ConfigFile returns the path of the OpenClaw config file
// (~/.openclaw/openclaw.json). The adapter surgically merges a small set
// of owned keys into it (plugins.load.paths, plugins.entries.waired,
// agents.defaults.models.<waired/*>) and removes exactly those on unlink.
func ConfigFile(home string) string {
	return filepath.Join(ConfigDir(home), "openclaw.json")
}

// PluginDir returns the directory of the waired-authored OpenClaw plugin
// (~/.openclaw/plugins/waired). OpenClaw does not auto-scan this tree, so
// the directory is registered in openclaw.json via plugins.load.paths.
func PluginDir(home string) string {
	return filepath.Join(ConfigDir(home), "plugins", "waired")
}
