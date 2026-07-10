package trust

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSecurity swaps runSecurity for a recorder and restores it on cleanup.
func fakeSecurity(t *testing.T, out string, err error) *[][]string {
	t.Helper()
	var calls [][]string
	orig := runSecurity
	runSecurity = func(args ...string) (string, error) {
		calls = append(calls, args)
		return out, err
	}
	t.Cleanup(func() { runSecurity = orig })
	return &calls
}

func fakeLaunchctl(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	orig := runLaunchctl
	runLaunchctl = func(args ...string) error {
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { runLaunchctl = orig })
	return &calls
}

func TestUninstallCAMatchesByCommonName(t *testing.T) {
	calls := fakeSecurity(t, "", nil)
	if err := UninstallCA(); err != nil {
		t.Fatalf("UninstallCA: %v", err)
	}
	got := (*calls)[0]
	want := []string{"delete-certificate", "-c", CACommonName, systemKeychain}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("delete-certificate argv = %v, want %v", got, want)
	}
}

func TestUninstallCANoMatchIsSuccess(t *testing.T) {
	fakeSecurity(t, "SecKeychainItemDelete: Unable to delete certificate matching \"x\"", errors.New("exit 1"))
	if err := UninstallCA(); err != nil {
		t.Fatalf("UninstallCA on no-match should succeed, got %v", err)
	}
}

// TestUninstallNodeExtraCAStripsBlock verifies the legacy-cleanup path strips
// the waired NODE_EXTRA_CA_CERTS block from /etc/zshenv (leaving operator
// content intact) and clears the launchd GUI session value.
func TestUninstallNodeExtraCAStripsBlock(t *testing.T) {
	dir := t.TempDir()
	zshenvFile = filepath.Join(dir, "zshenv")
	const preexisting = "export PATH=/usr/local/bin:$PATH"
	const certPath = "/Users/x/Library/Application Support/waired/proxy/ca.crt"
	// Seed an /etc/zshenv as the retired installer would have written it:
	// operator content + a marker-delimited waired block.
	seeded := preexisting + "\n" +
		zshenvBlockBegin + "\n" +
		"export NODE_EXTRA_CA_CERTS=\"" + certPath + "\"\n" +
		zshenvBlockEnd + "\n"
	if err := os.WriteFile(zshenvFile, []byte(seeded), 0o644); err != nil {
		t.Fatal(err)
	}
	lc := fakeLaunchctl(t)

	if err := UninstallNodeExtraCA(); err != nil {
		t.Fatalf("UninstallNodeExtraCA: %v", err)
	}
	b, _ := os.ReadFile(zshenvFile)
	got := string(b)
	if strings.Contains(got, "NODE_EXTRA_CA_CERTS") || strings.Contains(got, zshenvBlockBegin) {
		t.Fatalf("uninstall left waired block behind:\n%s", got)
	}
	if !strings.Contains(got, preexisting) {
		t.Fatalf("uninstall removed pre-existing content:\n%s", got)
	}
	if len(*lc) != 1 || (*lc)[0][0] != "unsetenv" || (*lc)[0][1] != nodeExtraCAEnv {
		t.Fatalf("launchctl calls = %v, want a single unsetenv %s", *lc, nodeExtraCAEnv)
	}
}
