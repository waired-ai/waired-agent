//go:build darwin

package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnable_WritesPlistAndCallsBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var seenArgs [][]string
	orig := runLaunchctlFn
	runLaunchctlFn = func(args []string) ([]byte, []byte, error) {
		seenArgs = append(seenArgs, append([]string(nil), args...))
		return nil, nil, nil
	}
	t.Cleanup(func() { runLaunchctlFn = orig })

	m := newManager("waired-tray")
	if err := m.Enable("/usr/local/bin/waired-tray",
		[]string{"--mgmt", "http://127.0.0.1:9476"}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Plist file should have been written under $HOME/Library/LaunchAgents.
	plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents",
		"com.waired.tray.waired-tray.plist")
	body, err := os.ReadFile(plist)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`<string>com.waired.tray.waired-tray</string>`,
		`<string>/usr/local/bin/waired-tray</string>`,
		`<string>--mgmt</string>`,
		`<string>http://127.0.0.1:9476</string>`,
		`<key>RunAtLoad</key>`,
		`<true/>`,
		`<key>KeepAlive</key>`,
		`<false/>`,
		`<key>ProcessType</key>`,
		`<string>Interactive</string>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q\n--- got ---\n%s", want, s)
		}
	}

	// We expect at least one bootout (best-effort cleanup) followed
	// by a bootstrap.
	var sawBootstrap bool
	for _, a := range seenArgs {
		if len(a) > 0 && a[0] == "bootstrap" && strings.HasSuffix(a[len(a)-1], ".plist") {
			sawBootstrap = true
		}
	}
	if !sawBootstrap {
		t.Errorf("no bootstrap call recorded; calls=%v", seenArgs)
	}
}

func TestDisable_RemovesPlistAndCallsBootout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents",
		"com.waired.tray.waired-tray.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sawBootout bool
	orig := runLaunchctlFn
	runLaunchctlFn = func(args []string) ([]byte, []byte, error) {
		if args[0] == "bootout" {
			sawBootout = true
		}
		return nil, nil, nil
	}
	t.Cleanup(func() { runLaunchctlFn = orig })

	if err := newManager("waired-tray").Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist should be gone after Disable; got err=%v", err)
	}
	if !sawBootout {
		t.Errorf("expected bootout call")
	}
}

func TestDisable_ToleratesMissingPlist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orig := runLaunchctlFn
	runLaunchctlFn = func(args []string) ([]byte, []byte, error) { return nil, nil, nil }
	t.Cleanup(func() { runLaunchctlFn = orig })
	if err := newManager("waired-tray").Disable(); err != nil {
		t.Errorf("Disable on clean host: %v", err)
	}
}

func TestEnable_PropagatesBootstrapFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	orig := runLaunchctlFn
	runLaunchctlFn = func(args []string) ([]byte, []byte, error) {
		if args[0] == "bootstrap" {
			return nil, []byte("Bootstrap failed: 5: Input/output error"),
				errors.New("exit status 5")
		}
		return nil, nil, nil
	}
	t.Cleanup(func() { runLaunchctlFn = orig })

	err := newManager("waired-tray").Enable("/usr/local/bin/waired-tray", nil)
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Errorf("expected bootstrap failure to propagate, got %v", err)
	}
}

func TestLabelIncludesAppName(t *testing.T) {
	got := (darwinManager{appName: "alpha"}).label()
	if got != "com.waired.tray.alpha" {
		t.Errorf("label = %q, want com.waired.tray.alpha", got)
	}
}

func TestPlistPath_RejectsEmptyAppName(t *testing.T) {
	_, err := (darwinManager{appName: ""}).plistPath()
	if err == nil {
		t.Error("plistPath with empty appName: want error")
	}
}
