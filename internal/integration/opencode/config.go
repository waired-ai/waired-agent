package opencode

import "path/filepath"

// ConfigDir returns the OpenCode config directory under home
// (~/.config/opencode).
func ConfigDir(home string) string {
	return filepath.Join(home, ".config", "opencode")
}

// CommandsDir returns the OpenCode global commands directory.
func CommandsDir(home string) string {
	return filepath.Join(ConfigDir(home), "commands")
}

// CommandFile returns the on-disk path of one waired-* command file.
func CommandFile(home, name string) string {
	return filepath.Join(CommandsDir(home), name+".md")
}
