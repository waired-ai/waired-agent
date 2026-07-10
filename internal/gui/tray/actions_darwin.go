//go:build darwin

package tray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// On macOS the agent is a system LaunchDaemon (root, /Library/LaunchDaemons,
// state under the root-owned /Library/Application Support/waired — #520), so
// the privileged tray actions need administrator elevation, matching Linux
// (pkexec → /var/lib/waired + /etc/systemd) and Windows (UAC →
// %ProgramData%\Waired + SCM). We elevate via osascript's `do shell script …
// with administrator privileges`, which pops the native macOS admin-auth
// dialog (runOsascriptAdmin below).
//
//   - Logout / Ollama-install run a one-shot command and just need root —
//     osascript fits cleanly.
//   - Sign-in is daemon-driven (tray.startLogin → the daemon's loopback
//     login API; the daemon owns the root state dir and the tray opens the
//     returned URL). LoginViaElevation is only the legacy 404 fallback and
//     cannot be reproduced as a GUI-elevated root process — see its doc.
//
// The function names keep the *Elevation suffix for cross-OS callsite
// symmetry: tray.go calls them without caring which mechanism each backend
// uses.

// LoginViaElevation is the legacy fallback tray.startLogin uses only when
// the daemon does not expose the loopback login API (HTTP 404 →
// ErrLoginUnsupported). A #520 daemon always exposes it, so on a working
// macOS install sign-in is daemon-driven and this is never reached. We
// cannot reproduce the interactive, URL-streaming login as a GUI-elevated
// root process — osascript's `do shell script` returns stdout only after
// completion, too late to open the login URL, and a sudo hop has no TTY to
// prompt for a password — so rather than fake it we point the user at the
// terminal / daemon path. The parameters are unused; kept for the cross-OS
// signature tray.go calls.
func LoginViaElevation(_ context.Context, _, _ string) error {
	return errors.New("sign-in needs the waired-agent daemon (it was not reachable). " +
		"Start it with `sudo launchctl kickstart -k system/com.waired.agent`, " +
		"or sign in from a terminal with `sudo waired init`")
}

// LogoutViaElevation runs `waired logout --yes --state-dir <dir>` as root
// via osascript admin: the state dir + System keychain it wipes are
// root-owned (#520). --yes skips the CLI's interactive y/N because the
// tray has its own ConfirmYesNo wrapper around this call. logout touches
// only the (root) state dir and CP deauth — no user-home component — so
// root alone is sufficient.
func LogoutViaElevation(ctx context.Context, stateDir string) error {
	bin, err := locateWairedBinary()
	if err != nil {
		return err
	}
	shellCmd := shellQuote(bin) + " logout --yes --state-dir " + shellQuote(stateDir)
	if err := runOsascriptAdmin(ctx, shellCmd); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// InstallOllamaViaElevation runs `waired runtimes install ollama -y` as
// root via osascript admin: the bundled engine installs under the
// root-owned <state-dir>/runtimes/ollama and is launched by the root
// daemon (#520). If the waired binary cannot be located we fall back to
// opening the Ollama download page. (#188)
func InstallOllamaViaElevation(ctx context.Context, stateDir string) error {
	bin, err := locateWairedBinary()
	if err != nil {
		if oerr := OpenBrowser("https://ollama.com/download"); oerr != nil {
			return fmt.Errorf("install: waired binary not found and could not open browser: %w", oerr)
		}
		return nil
	}
	shellCmd := shellQuote(bin) + " runtimes install ollama -y"
	if stateDir != "" {
		shellCmd += " --state-dir " + shellQuote(stateDir)
	}
	if err := runOsascriptAdmin(ctx, shellCmd); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

// UpdateViaElevation runs `waired update --yes` as root. The apply
// re-runs install.sh which rewrites /usr/local/bin and re-registers the
// system LaunchDaemon, so it needs root. Signing-free: install.sh
// curl-downloads the tarball, which carries no Gatekeeper quarantine
// attribute. (#293)
func UpdateViaElevation(ctx context.Context) error {
	bin, err := locateWairedBinary()
	if err != nil {
		if oerr := OpenBrowser("https://github.com/waired-ai/waired-install"); oerr != nil {
			return fmt.Errorf("update: waired binary not found and could not open browser: %w", oerr)
		}
		return nil
	}
	if err := runOsascriptAdmin(ctx, shellQuote(bin)+" update --yes"); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// runOsascriptAdmin runs shellCmd as root via osascript's `do shell
// script … with administrator privileges`, which pops the native macOS
// admin-auth dialog. `do shell script` runs under a minimal PATH that
// excludes /usr/local/bin, so callers must pass absolute binary paths
// (shellQuote'd).
func runOsascriptAdmin(ctx context.Context, shellCmd string) error {
	script := "do shell script " + quoteAppleScript(shellCmd) + " with administrator privileges"
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// shellQuote single-quotes s for POSIX sh, so paths/args with spaces or
// metacharacters survive the `do shell script` round-trip.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// locateWairedBinary finds the absolute path to the `waired` CLI. On
// macOS the canonical install is /usr/local/bin/waired (Homebrew-style)
// or /Applications/Waired.app/Contents/Resources/waired (future .app
// bundle). For a hand-built developer environment we fall back to PATH.
func locateWairedBinary() (string, error) {
	candidates := []string{
		"/usr/local/bin/waired",
		"/opt/homebrew/bin/waired",
		"/Applications/Waired.app/Contents/Resources/waired",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	p, err := exec.LookPath("waired")
	if err != nil {
		return "", fmt.Errorf("waired not found in /usr/local/bin, /opt/homebrew/bin, /Applications/Waired.app, or PATH: %w", err)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs, nil
	}
	return p, nil
}

// wairedCLIPath finds the `waired` CLI binary the tray shells out to for
// `waired codeui …`. Same resolution as the elevation helpers.
func wairedCLIPath() (string, error) { return locateWairedBinary() }

// OpenBrowser launches url with the user's default handler. macOS
// ships `open(1)` which is the canonical way to invoke
// LaunchServices — equivalent to xdg-open on Linux / ShellExecute on
// Windows.
func OpenBrowser(url string) error {
	if url == "" {
		return errors.New("OpenBrowser: empty url")
	}
	cmd := exec.Command("/usr/bin/open", url)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// CopyToClipboard pipes text to /usr/bin/pbcopy. Trailing CRLF is
// trimmed to match Linux/Windows behaviour where Ctrl+V into a text
// field should not paste a stray newline.
func CopyToClipboard(text string) error {
	cmd := exec.Command("/usr/bin/pbcopy")
	cmd.Stdin = strings.NewReader(strings.TrimRight(text, "\r\n"))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipboard (pbcopy): %w", err)
	}
	return nil
}

// ShowAbout displays the About dialog. macOS native UI is via
// osascript's `display dialog` — light enough that we do not need a
// native NSAlert + CGO. Falls through to stderr if osascript itself
// is missing (impossible on a normal macOS install, but defensive).
func ShowAbout(version, sha string) {
	body := fmt.Sprintf("Waired %s\nbuild %s\n\nhttps://github.com/waired-ai/waired", version, sha)
	if !runOsascriptDialog("About Waired", body, "note", []string{"OK"}, "OK") {
		fmt.Fprintln(os.Stderr, body)
	}
}

// ShowError surfaces a recoverable problem (failed login subprocess,
// missing waired binary on PATH, etc.).
func ShowError(message string) {
	if !runOsascriptDialog("Waired", message, "stop", []string{"OK"}, "OK") {
		fmt.Fprintln(os.Stderr, "waired-tray:", message)
	}
}

// ShowConfirm asks the user to confirm a destructive action. Returns
// true only when the user clicked the affirmative button. If the
// dialog itself cannot be shown (osascript missing or DISPLAY-less
// SSH session) we err on the side of cancel.
func ShowConfirm(prompt string) bool {
	pressed, ok := runOsascriptDialogReturning("Waired", prompt, "caution",
		[]string{"Cancel", "OK"}, "Cancel")
	if !ok {
		return false
	}
	return pressed == "OK"
}

// runOsascriptDialog shows a dialog and returns true if osascript
// itself was available and exited (the user clicked one of the
// buttons). Used by the no-return-value paths (ShowAbout / ShowError).
func runOsascriptDialog(title, body, icon string, buttons []string, defaultButton string) bool {
	_, ok := runOsascriptDialogReturning(title, body, icon, buttons, defaultButton)
	return ok
}

// runOsascriptDialogReturning runs `display dialog` and returns the
// button that the user pressed (the empty string when osascript could
// not be invoked or the user dismissed the dialog without picking).
//
// We build the AppleScript on the fly because `osascript` does not
// have a way to pass dialog parameters as separate argv entries. The
// quoting routine doubles every backslash and double-quote so a
// pathological body (e.g. embedded "Library/" with escaped quotes)
// renders correctly.
func runOsascriptDialogReturning(title, body, icon string, buttons []string, defaultButton string) (string, bool) {
	var script bytes.Buffer
	script.WriteString(`display dialog `)
	script.WriteString(quoteAppleScript(body))
	script.WriteString(` with title `)
	script.WriteString(quoteAppleScript(title))
	if icon != "" {
		script.WriteString(` with icon `)
		script.WriteString(icon)
	}
	if len(buttons) > 0 {
		script.WriteString(` buttons {`)
		for i, b := range buttons {
			if i > 0 {
				script.WriteString(", ")
			}
			script.WriteString(quoteAppleScript(b))
		}
		script.WriteString(`}`)
	}
	if defaultButton != "" {
		script.WriteString(` default button `)
		script.WriteString(quoteAppleScript(defaultButton))
	}

	cmd := exec.Command("/usr/bin/osascript", "-e", script.String())
	out, err := cmd.Output()
	if err != nil {
		// osascript exits non-zero when the user pressed the script's
		// "Cancel" button via Esc / Cmd-. ; treat that as
		// "no selection." Other errors (osascript missing) we also
		// return false from.
		return "", false
	}
	// osascript prints: `button returned:OK, gave up:false` (with a
	// trailing newline). We only care about the button name.
	s := strings.TrimSpace(string(out))
	if i := strings.Index(s, "button returned:"); i >= 0 {
		rest := s[i+len("button returned:"):]
		if j := strings.Index(rest, ","); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest), true
	}
	return "", true
}

// quoteAppleScript returns the literal AppleScript form of s — that
// is, the input wrapped in double quotes with every backslash and
// double-quote escaped. AppleScript follows the same convention as C
// strings here.
func quoteAppleScript(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
