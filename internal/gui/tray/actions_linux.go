//go:build linux

package tray

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// LoginViaElevation (Linux: pkexec) spawns `waired init --no-browser
// --control <url> --state-dir <dir>` under polkit elevation so the
// elevated CLI can write to /var/lib/waired, reads the login URL out
// of its stdout, and opens it via xdg-open in the desktop user's
// session (xdg-open from inside pkexec misbehaves because the
// elevated context loses DISPLAY / XDG_RUNTIME_DIR / keyring).
//
// The function blocks until the subprocess exits — the caller runs it
// from a goroutine so the systray event loop is not blocked. After it
// returns, callers should re-poll /v1/identity to learn the outcome.
//
// The "Elevation" suffix is OS-agnostic so the Windows sibling
// (UAC RunAs via ShellExecuteEx) can use the same name from tray.go.
func LoginViaElevation(ctx context.Context, controlURL, stateDir string) error {
	if controlURL == "" {
		return errors.New("login: --control URL is empty (set WAIRED_CONTROL_URL or pass via flag)")
	}
	args := []string{
		"waired", "init",
		"--no-browser",
		"--state-dir", stateDir,
		"--control", controlURL,
		"--skip-deploy",      // tray does not need the LLM-deploy phase
		"--skip-integration", // tray does not need shell-rc + Claude/OpenClaw mutation
	}
	cmd := exec.CommandContext(ctx, "pkexec", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("login: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("login: start pkexec: %w", err)
	}

	// Stream stdout and watch for the login URL. The CLI prints:
	//   Open this URL on another device:
	//     <url>
	//
	//   Code: <code>
	go pumpLoginURL(stdout)

	if err := cmd.Wait(); err != nil {
		// pkexec exits 126 when the user cancels the auth dialog, 127 when
		// the action is denied. Surface a friendly error, not the raw exit code.
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			switch ee.ExitCode() {
			case 126:
				return errors.New("login: authentication cancelled")
			case 127:
				return errors.New("login: not authorized to run waired init")
			}
		}
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

// StartAgentServiceViaElevation starts the waired-agent systemd unit,
// elevating with pkexec.
//
// It runs systemctl rather than `waired-agent start`, for two reasons: it is
// exactly what linuxManager.Start does (service_linux.go), and it needs no
// locator for the agent binary — whose install path differs between the .deb
// (/usr/bin) and install.sh (/usr/local/bin). pkexec needs an absolute path
// either way, since polkit matches its actions on the program path.
//
// There is no waired-specific polkit action for this, so polkit falls back to
// org.freedesktop.policykit.exec and asks for an administrator password each
// time. That is the intended behaviour for now; a dedicated action with a
// softer allow_active is a security-posture decision that belongs with the
// OS-consent design (waired#845), not here.
func StartAgentServiceViaElevation(ctx context.Context) error {
	if _, err := exec.LookPath("pkexec"); err != nil {
		return fmt.Errorf("start the agent: pkexec is not installed; run `%s` in a terminal", service.StartHint())
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return fmt.Errorf("start the agent: systemctl not found; run `%s` in a terminal", service.StartHint())
	}
	cmd := exec.CommandContext(ctx, "pkexec", systemctl, "start", service.ServiceName)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			switch ee.ExitCode() {
			case 126:
				return errors.New("start the agent: authentication cancelled")
			case 127:
				return errors.New("start the agent: not authorized to start the service")
			}
		}
		return fmt.Errorf("start the agent: %w", err)
	}
	return nil
}

// pumpLoginURL is a background goroutine that scans the CLI stdout
// for the "Open this URL" header and opens the next non-empty,
// indented line via xdg-open. We do this from the tray (the user's
// desktop session) rather than from inside pkexec.
func pumpLoginURL(stdout io.Reader) {
	br := bufio.NewReader(stdout)
	awaitURL := false
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trim := strings.TrimSpace(line)
			if awaitURL && strings.HasPrefix(trim, "http") {
				_ = OpenBrowser(trim)
				awaitURL = false
			}
			if strings.HasPrefix(trim, "Open this URL") {
				awaitURL = true
			}
			// Mirror to our stderr so the operator can see progress.
			fmt.Fprint(os.Stderr, line)
		}
		if err != nil {
			return
		}
	}
}

// LogoutViaElevation (Linux: pkexec) runs `waired logout --yes
// --state-dir <dir>`. The --yes skips the interactive y/N inside the
// CLI; the auth prompt happens at the polkit layer instead, which is
// the right place. The "Elevation" suffix matches LoginViaElevation
// so the Windows sibling can share the call site.
func LogoutViaElevation(ctx context.Context, stateDir string) error {
	cmd := exec.CommandContext(ctx, "pkexec", "waired", "logout", "--yes", "--state-dir", stateDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) && ee.ExitCode() == 126 {
			return errors.New("logout: authentication cancelled")
		}
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// InstallOllamaViaElevation (Linux: pkexec) runs `waired runtimes
// install ollama -y` under polkit elevation — the installer writes to
// /usr/local/bin and touches the system service, so it needs root. When
// no polkit agent is available we fall back to opening the Ollama
// download page in the user's browser. (#188)
func InstallOllamaViaElevation(ctx context.Context, stateDir string) error {
	if _, err := exec.LookPath("pkexec"); err != nil {
		if oerr := OpenBrowser("https://ollama.com/download"); oerr != nil {
			return fmt.Errorf("install: pkexec unavailable and could not open browser: %w", oerr)
		}
		return nil
	}
	args := []string{"waired", "runtimes", "install", "ollama", "-y"}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	cmd := exec.CommandContext(ctx, "pkexec", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			switch ee.ExitCode() {
			case 126:
				return errors.New("install: authentication cancelled")
			case 127:
				return errors.New("install: not authorized to run waired runtimes install")
			}
		}
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

// UpdateViaElevation (Linux: pkexec) runs `waired update --yes` under
// polkit elevation — the apply re-runs install.sh, which writes system
// paths and restarts the service, so it needs root; pkexec gives the GUI
// auth dialog the tray (no TTY) requires. When no polkit agent is
// available we fall back to opening the install mirror so the operator can
// upgrade by hand. (#293)
func UpdateViaElevation(ctx context.Context) error {
	if _, err := exec.LookPath("pkexec"); err != nil {
		if oerr := OpenBrowser("https://github.com/waired-ai/waired-agent"); oerr != nil {
			return fmt.Errorf("update: pkexec unavailable and could not open browser: %w", oerr)
		}
		return nil
	}
	cmd := exec.CommandContext(ctx, "pkexec", "waired", "update", "--yes")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee := (&exec.ExitError{}); errors.As(err, &ee) {
			switch ee.ExitCode() {
			case 126:
				return errors.New("update: authentication cancelled")
			case 127:
				return errors.New("update: not authorized to run waired update")
			}
		}
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// CopyToClipboard copies text into the clipboard, picking wl-copy on
// Wayland and xclip on X11. A failure is non-fatal — the menu builder
// just shows the failure via ShowError.
func CopyToClipboard(text string) error {
	var cmd *exec.Cmd
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		cmd = exec.Command("wl-copy")
	} else {
		cmd = exec.Command("xclip", "-selection", "clipboard")
	}
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clipboard (%s): %w (install wl-clipboard or xclip)", cmd.Path, err)
	}
	return nil
}

// ShowAbout displays a small info dialog. zenity is preferred on
// GNOME-derived environments, kdialog on KDE; if neither is installed
// the call falls back to printing on stderr (the user still has the
// inline About menu item with the version string).
func ShowAbout(version, sha string) {
	body := fmt.Sprintf("Waired %s\nbuild %s\n\nhttps://github.com/waired-ai/waired", version, sha)
	if tryDialog("--info", "About Waired", body) {
		return
	}
	fmt.Fprintln(os.Stderr, body)
}

// ShowError surfaces a problem that needs the user's attention (e.g.
// failed login subprocess). Same dialog chain as ShowAbout; when
// neither tool is installed it goes through errorFallback, which puts
// the message somewhere a person on a desktop can actually see it.
func ShowError(message string) {
	if tryDialog("--error", "Waired", message) {
		return
	}
	errorFallback(message)
}

// ShowConfirm asks for yes/no acknowledgement before a destructive
// action (currently only Log out). Returns false when no dialog tool
// is installed — destructive actions must err on the side of caution.
//
// --default-cancel for the same reason ConfirmWithLabels carries it
// (waired-ai/waired#901 L5, waired-agent#839): the dialog can take the
// foreground from whatever the user was typing into, and the keystroke
// that lands on it must not be the one that removes this device's
// identity. kdialog's --yesno has no default-button switch, so on that
// backend the default stays whatever KDE picks — the same limitation
// confirmLabelCandidates documents.
func ShowConfirm(prompt string) bool {
	for _, prog := range showConfirmCandidates(prompt) {
		path, err := exec.LookPath(prog.binary)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, prog.args...) //nolint:gosec // args are static, computed by us
		return cmd.Run() == nil
	}
	return false
}

// ShowStatus shows the status summary and reports whether the user asked
// for the full details on the clipboard.
//
// zenity's --question with relabelled buttons, then kdialog's, then
// nothing: on a box with neither installed there is no window to click,
// so the report goes to the clipboard unasked and a toast says where it
// went. That is the same three-channel reasoning error_fallback.go
// documents — the user asked to see something, and "no dialog backend"
// must not turn that into silence.
//
// --icon-name=dialog-information, not the question icon zenity defaults
// to: this box reports a state, and an alert on a healthy machine is the
// class of thing waired-agent#1032 was about.
//
// Blocks on the spawned process, like every dialog here: callers must
// invoke it from a goroutine.
func ShowStatus(body string) (copyRequested bool) {
	for _, prog := range showStatusCandidates(body) {
		path, err := exec.LookPath(prog.binary)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, prog.args...) //nolint:gosec // args are static, computed by us
		err = cmd.Run()
		if err == nil {
			return true
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// The Close button. The dialog worked; nothing to copy.
			return false
		}
		// The dialog itself failed (no DISPLAY, missing GTK). Try the
		// next backend before giving up on the whole window.
	}
	return statusFallback(body)
}

// showStatusCandidates is the argv ShowStatus spawns, split out so the
// button layout is assertable without a desktop (dialog_linux_test.go).
// Close is the default for the same reason ShowConfirm's cancel is: the
// dialog can take the foreground, and a stray Return must not overwrite
// the clipboard.
func showStatusCandidates(body string) []confirmProgram {
	return []confirmProgram{
		{
			binary: "zenity",
			args: []string{
				"--question",
				"--title=Waired status",
				"--text=" + body,
				"--icon-name=dialog-information",
				"--ok-label=Copy details",
				"--cancel-label=Close",
				"--default-cancel",
			},
		},
		{
			binary: "kdialog",
			args: []string{
				"--title", "Waired status",
				"--yes-label", "Copy details",
				"--no-label", "Close",
				"--yesno", body,
			},
		},
	}
}

// statusFallback is what a machine with no dialog backend gets. It
// returns true so the caller copies — there was no window in which to
// ask, and a report nobody can read is worse than a clipboard nobody
// asked for. The toast is what tells them it happened.
func statusFallback(body string) bool {
	slog.Warn("tray: no dialog backend for the status report; copying instead", "lines", strings.Count(body, "\n")+1)
	return true
}

// showConfirmCandidates is the argv ShowConfirm spawns, split out so the
// safe default is assertable without a desktop (dialog_linux_test.go).
// zenity first, then kdialog — the same preference order and the same
// backend limitation as confirmCandidates.
func showConfirmCandidates(prompt string) []confirmProgram {
	return []confirmProgram{
		{
			binary: "zenity",
			args:   []string{"--question", "--title", "Waired", "--text", prompt, "--default-cancel"},
		},
		{
			binary: "kdialog",
			args:   []string{"--yesno", prompt, "--title", "Waired"},
		},
	}
}

// tryDialog runs zenity or kdialog with the given mode flag. Returns
// true when one of them was present and exited cleanly.
func tryDialog(mode, title, body string) bool {
	if path, err := exec.LookPath("zenity"); err == nil {
		cmd := exec.Command(path, mode, "--title", title, "--text", body)
		return cmd.Run() == nil
	}
	if path, err := exec.LookPath("kdialog"); err == nil {
		// kdialog flag mapping: --info → --msgbox, --error → --error
		flag := "--msgbox"
		if mode == "--error" {
			flag = "--error"
		}
		cmd := exec.Command(path, flag, body, "--title", title)
		return cmd.Run() == nil
	}
	return false
}

// LinkIntegrationAsUser runs `waired link <target>` as this process's
// own user — deliberately NOT under pkexec. The plugin belongs to the
// desktop user's home; elevating would write it into root's
// (waired-agent#986).
func LinkIntegrationAsUser(ctx context.Context, target string) error {
	bin, err := exec.LookPath("waired")
	if err != nil {
		return fmt.Errorf("waired not found in PATH; run `waired link %s` in a terminal: %w", target, err)
	}
	return runWairedLink(ctx, bin, target)
}
