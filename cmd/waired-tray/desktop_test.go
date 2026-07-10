package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// appLauncherDesktopPath is the repo-relative path to the
// /usr/share/applications launcher shipped by the waired-tray .deb (see
// packaging/nfpm/waired-tray.yaml.tmpl) and the manual installer
// (build/install-desktop.sh). `go test` runs with the working directory
// set to this package's directory, so the repo root is two levels up.
const appLauncherDesktopPath = "../../build/applications/waired-tray.desktop"

// parseDesktopEntry reads a freedesktop .desktop file and returns the
// key=value pairs under the [Desktop Entry] group, preserving empty
// values (so we can assert that keys like OnlyShowIn are absent rather
// than present-but-empty).
func parseDesktopEntry(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	entries := map[string]string{}
	inGroup := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inGroup = trimmed == "[Desktop Entry]"
			continue
		}
		if !inGroup {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed line in %s: %q", path, line)
		}
		entries[strings.TrimSpace(k)] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return entries
}

// TestAppLauncherDesktopFile guards the /usr/share/applications launcher
// added for issue #492. In particular it pins Exec to a PATH-relative
// command so the entry works for both the .deb (binary at /usr/bin) and
// the manual installer (/usr/local/bin) — the absolute-path mistake that
// broke the autostart entry (#491) must not recur here.
func TestAppLauncherDesktopFile(t *testing.T) {
	e := parseDesktopEntry(t, appLauncherDesktopPath)

	// Required keys for a valid, discoverable application launcher.
	for _, k := range []string{"Type", "Name", "Exec"} {
		if strings.TrimSpace(e[k]) == "" {
			t.Errorf("%s: missing or empty %q", appLauncherDesktopPath, k)
		}
	}

	if got := e["Type"]; got != "Application" {
		t.Errorf("Type = %q, want %q", got, "Application")
	}

	// Exec must be PATH-relative (no leading '/' and no directory
	// component) so it resolves under both /usr/bin and /usr/local/bin.
	exec := strings.TrimSpace(e["Exec"])
	if exec != "waired-tray" {
		t.Errorf("Exec = %q, want PATH-relative %q (absolute paths break across install layouts, see #491)", exec, "waired-tray")
	}

	// Must be categorised under Network to land in the right app-grid
	// section.
	cats := e["Categories"]
	if !strings.Contains(cats, "Network") {
		t.Errorf("Categories = %q, want to contain %q", cats, "Network")
	}

	// An empty OnlyShowIn / NotShowIn is implementation-defined ("show in
	// no environment" on some readers) — these keys must be omitted, not
	// present-but-empty.
	for _, k := range []string{"OnlyShowIn", "NotShowIn"} {
		if v, ok := e[k]; ok && strings.TrimSpace(v) == "" {
			t.Errorf("%s must be omitted when empty, found %q=", appLauncherDesktopPath, k)
		}
	}
}
