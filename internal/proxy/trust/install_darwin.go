//go:build darwin

package trust

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	// systemKeychain is the machine-wide trust store; adding a trustRoot here
	// requires root (the `waired proxy install` caller is root via sudo).
	systemKeychain = "/Library/Keychains/System.keychain"
	// nodeExtraCAEnv is the Node.js variable Claude Code honors to trust an
	// extra CA bundle (Node ignores the OS trust store).
	nodeExtraCAEnv = "NODE_EXTRA_CA_CERTS"
)

// zshenvFile is the file the NODE_EXTRA_CA_CERTS block is written to. A var so
// tests can point it at a temp file. /etc/zshenv is sourced by EVERY zsh
// invocation (login/interactive/non-interactive), the broadest coverage on a
// default-zsh macOS.
var zshenvFile = "/etc/zshenv"

// zshenv block markers delimit the NODE_EXTRA_CA_CERTS export so uninstall can
// strip exactly its own lines. Darwin-only (kept here, not in the shared
// trust.go, so non-darwin builds don't flag them unused).
const (
	zshenvBlockBegin = "# >>> waired claude proxy (NODE_EXTRA_CA_CERTS) >>>"
	zshenvBlockEnd   = "# <<< waired claude proxy (NODE_EXTRA_CA_CERTS) <<<"
)

// runSecurity runs /usr/bin/security and returns combined output + error. A
// package var so tests assert argv without invoking the real keychain.
var runSecurity = func(args ...string) (string, error) {
	out, err := exec.Command("/usr/bin/security", args...).CombinedOutput()
	return string(out), err
}

// runLaunchctl runs /bin/launchctl. Best-effort env propagation to the launchd
// GUI session for GUI-launched Claude Code; a package var for tests.
var runLaunchctl = func(args ...string) error {
	return exec.Command("/bin/launchctl", args...).Run()
}

// UninstallCA removes the proxy CA from the System keychain, matched by
// CommonName so no on-disk PEM path is needed (the shared trust.UninstallCA
// signature is argless). A no-match is success — the CA is already absent.
func UninstallCA() error {
	out, err := runSecurity("delete-certificate", "-c", CACommonName, systemKeychain)
	if err != nil {
		lower := strings.ToLower(out)
		if strings.Contains(lower, "unable to delete") ||
			strings.Contains(lower, "could not be found") ||
			strings.Contains(lower, "no matching") {
			return nil
		}
		return fmt.Errorf("trust: security delete-certificate: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// UninstallNodeExtraCA strips the /etc/zshenv block and clears the launchd GUI
// session value. Missing block / value is not an error.
func UninstallNodeExtraCA() error {
	if err := removeZshenvBlock(); err != nil {
		return err
	}
	_ = runLaunchctl("unsetenv", nodeExtraCAEnv)
	return nil
}

// removeZshenvBlock rewrites zshenvFile without the waired block, leaving any
// other content intact. A missing file is a no-op.
func removeZshenvBlock() error {
	if _, err := os.Stat(zshenvFile); os.IsNotExist(err) {
		return nil
	}
	base, err := strippedZshenv()
	if err != nil {
		return err
	}
	body := base
	if body != "" {
		body += "\n"
	}
	if err := os.WriteFile(zshenvFile, []byte(body), 0o644); err != nil {
		return fmt.Errorf("trust: write %s: %w", zshenvFile, err)
	}
	return nil
}

// strippedZshenv returns the current zshenvFile content with any waired block
// (begin..end markers, inclusive) removed and trailing newlines trimmed. A
// missing file yields "".
func strippedZshenv() (string, error) {
	b, err := os.ReadFile(zshenvFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("trust: read %s: %w", zshenvFile, err)
	}
	lines := strings.Split(string(b), "\n")
	kept := make([]string, 0, len(lines))
	inBlock := false
	for _, ln := range lines {
		switch strings.TrimSpace(ln) {
		case zshenvBlockBegin:
			inBlock = true
			continue
		case zshenvBlockEnd:
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n"), nil
}
