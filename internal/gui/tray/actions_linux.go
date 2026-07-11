//go:build linux

package tray

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
		"--skip-integration", // tray does not need shell-rc + Claude/OpenCode mutation
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

// wairedCLIPath finds the `waired` CLI binary (distinct from waired-tray) the
// tray shells out to for `waired codeui …`. PATH first (the installer puts
// waired in /usr/bin or /usr/local/bin), then the canonical install dirs.
func wairedCLIPath() (string, error) {
	if p, err := exec.LookPath("waired"); err == nil {
		return p, nil
	}
	for _, c := range []string{"/usr/bin/waired", "/usr/local/bin/waired"} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("waired CLI not found in PATH, /usr/bin, or /usr/local/bin")
}

// OpenBrowser launches the user's preferred handler for url. xdg-open
// is the only standard on Linux desktops; we don't try Wayland-specific
// hacks here because xdg-open is a thin shell over .desktop MIME
// resolution that Wayland sessions also honor.
func OpenBrowser(url string) error {
	cmd := exec.Command("xdg-open", url)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
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
// failed login subprocess). Same fallback chain as ShowAbout.
func ShowError(message string) {
	if tryDialog("--error", "Waired", message) {
		return
	}
	fmt.Fprintln(os.Stderr, "waired-tray:", message)
}

// ShowConfirm asks for yes/no acknowledgement before a destructive
// action (currently only Log out). Returns false when no dialog tool
// is installed — destructive actions must err on the side of caution.
func ShowConfirm(prompt string) bool {
	if path, err := exec.LookPath("zenity"); err == nil {
		cmd := exec.Command(path, "--question", "--title", "Waired", "--text", prompt)
		return cmd.Run() == nil
	}
	if path, err := exec.LookPath("kdialog"); err == nil {
		cmd := exec.Command(path, "--yesno", prompt, "--title", "Waired")
		return cmd.Run() == nil
	}
	return false
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
