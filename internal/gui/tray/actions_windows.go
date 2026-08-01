//go:build windows

package tray

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// LoginViaElevation / LogoutViaElevation spawn an elevated waired.exe
// via ShellExecuteW with the "runas" verb, which triggers the
// Windows UAC consent dialog. The elevated process can't share its
// stdout pipe across the privilege boundary, so we do NOT pass
// --no-browser; the elevated waired init opens the user's default
// browser itself (this works because HKCU MIME associations are
// effective from elevated contexts too). The tray detects login
// completion by polling /v1/identity on its existing 5 s tick.
//
// Why a fixed exe-path lookup instead of PATH: ShellExecuteW with
// the "runas" verb resolves the program string in the OS shell, and
// the elevated context has a different PATH than the user's tray
// session. We probe %ProgramFiles%\Waired first (the canonical
// install dir laid down by scripts/install/waired-agent-windows.ps1),
// then fall back to PATH lookup so a developer running the tray
// against a hand-built waired.exe is not blocked.
func LoginViaElevation(ctx context.Context, controlURL, stateDir string) error {
	if controlURL == "" {
		return errors.New("login: --control URL is empty (set WAIRED_CONTROL_URL or pass via flag)")
	}
	exe, err := locateWairedExe()
	if err != nil {
		return err
	}
	args := []string{
		"init",
		"--state-dir", stateDir,
		"--control", controlURL,
		"--skip-deploy",
		"--skip-integration",
	}
	return shellExecuteRunAs(ctx, "login", exe, args)
}

// LogoutViaElevation runs `waired logout --yes --state-dir <dir>`
// under UAC elevation. --yes skips the CLI's interactive prompt; the
// confirmation gate is the UAC dialog itself.
func LogoutViaElevation(ctx context.Context, stateDir string) error {
	exe, err := locateWairedExe()
	if err != nil {
		return err
	}
	args := []string{"logout", "--yes", "--state-dir", stateDir}
	return shellExecuteRunAs(ctx, "logout", exe, args)
}

// InstallOllamaViaElevation runs `waired runtimes install ollama -y`
// under UAC elevation; the embedded Windows installer writes under
// %ProgramFiles%\Ollama, which requires Administrator. When waired.exe
// cannot be located we fall back to the Ollama download page. (#188)
func InstallOllamaViaElevation(ctx context.Context, stateDir string) error {
	exe, err := locateWairedExe()
	if err != nil {
		if oerr := OpenBrowser("https://ollama.com/download"); oerr != nil {
			return fmt.Errorf("install: waired.exe not found and could not open browser: %w", err)
		}
		return nil
	}
	args := []string{"runtimes", "install", "ollama", "-y"}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	return shellExecuteRunAs(ctx, "install Ollama", exe, args)
}

// UpdateViaElevation runs `waired update --yes` under UAC elevation. The
// CLI re-runs install.ps1, whose elevated phase swaps the binaries under
// %ProgramFiles%\Waired and restarts the SCM service. Launching via the
// "runas" verb gives the elevated, non-console process install.ps1's swap
// needs. When waired.exe cannot be located we fall back to the install
// mirror page. (#293)
func UpdateViaElevation(ctx context.Context) error {
	exe, err := locateWairedExe()
	if err != nil {
		if oerr := OpenBrowser("https://github.com/waired-ai/waired-agent"); oerr != nil {
			return fmt.Errorf("update: waired.exe not found and could not open browser: %w", err)
		}
		return nil
	}
	return shellExecuteRunAs(ctx, "update", exe, []string{"update", "--yes"})
}

// StartAgentServiceViaElevation starts the waired-agent service through the
// SCM, elevating with UAC.
//
// It launches sc.exe rather than `waired-agent.exe start`, and that is the
// whole point. #315's failure is Windows Smart App Control blocking the
// *unsigned* waired-agent.exe when the SCM tries to start it at boot; asking
// the user to elevate that same unsigned binary invites the same block, and
// hands them a UAC dialog reading "Publisher: Unknown". sc.exe ships with
// Windows and is Microsoft-signed, so SAC never questions the launcher and the
// prompt names the Service Control Manager. The service binary is then started
// by the SCM exactly as it is at boot — which is also the code path we want to
// exercise, since that is the one that failed.
//
// Absolute path from the system directory, not a PATH lookup: the elevated
// process gets a different PATH than the tray session, and "the thing named
// sc.exe on some PATH" is not what we mean.
func StartAgentServiceViaElevation(ctx context.Context) error {
	sc, err := systemExe("sc.exe")
	if err != nil {
		return fmt.Errorf("start the agent: %w", err)
	}
	return shellExecuteRunAs(ctx, "start the agent", sc, []string{"start", service.ServiceName})
}

// systemExe resolves a Windows-supplied tool inside %SystemRoot%\System32.
func systemExe(name string) (string, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("locate the Windows system directory: %w", err)
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%s not found at %s: %w", name, p, err)
	}
	return p, nil
}

// locateWairedExe finds the absolute path to waired.exe to feed
// ShellExecuteW. Checks %ProgramFiles%\Waired\waired.exe first (the
// canonical install location), then falls back to PATH.
func locateWairedExe() (string, error) {
	pf := os.Getenv("ProgramFiles")
	if pf != "" {
		candidate := filepath.Join(pf, "Waired", "waired.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	p, err := exec.LookPath("waired.exe")
	if err != nil {
		return "", fmt.Errorf("waired.exe not found in %%ProgramFiles%%\\Waired or PATH: %w", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p, nil
	}
	return abs, nil
}

// wairedCLIPath finds the `waired` CLI binary the tray shells out to for
// `waired codeui …`. Reuses the elevation-helper locator.
func wairedCLIPath() (string, error) { return locateWairedExe() }

// quoteArgsForShellExec joins args into a single command-line string
// using Win32's standard "quote when the arg contains space/tab/quote"
// convention. CreateProcess (which ShellExecute eventually calls)
// parses the params string this way.
func quoteArgsForShellExec(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"") {
			parts = append(parts, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}

// shellExecuteRunAs invokes ShellExecuteW(0, "runas", exe, params,
// NULL, SW_SHOWNORMAL). Returns nil on the UAC consent + spawn
// success path; an error describing the user's "Cancel" or system
// failure otherwise. action names the caller ("login", "update",
// "start the agent") and prefixes those errors: the prefix used to be
// hardcoded "login", so a failed service start reported itself as a
// sign-in failure. We deliberately do not wait for the elevated
// process to exit (parent → elevated stdout is not pipe-able); the
// tray observes completion by polling /v1/identity.
func shellExecuteRunAs(_ context.Context, action, exe string, args []string) error {
	verb, _ := windows.UTF16PtrFromString("runas")
	exeW, _ := windows.UTF16PtrFromString(exe)
	paramsW, _ := windows.UTF16PtrFromString(quoteArgsForShellExec(args))
	procShellExecute := shell32.NewProc("ShellExecuteW")
	r, _, _ := procShellExecute.Call(
		0,                                // hwnd
		uintptr(unsafe.Pointer(verb)),    // lpOperation
		uintptr(unsafe.Pointer(exeW)),    // lpFile
		uintptr(unsafe.Pointer(paramsW)), // lpParameters
		0,                                // lpDirectory
		uintptr(1),                       // SW_SHOWNORMAL
	)
	// ShellExecuteW returns an HINSTANCE-shaped value: > 32 means
	// success; specific small codes describe the failure mode.
	if r > 32 {
		return nil
	}
	switch r {
	case 5, 0:
		// 5: SE_ERR_ACCESSDENIED (UAC cancelled by user).
		return fmt.Errorf("%s: UAC consent declined", action)
	case 2:
		return fmt.Errorf("%s: %s not found", action, exe)
	case 3:
		return fmt.Errorf("%s: path not found", action)
	case 8:
		return fmt.Errorf("%s: out of memory", action)
	default:
		return fmt.Errorf("%s: ShellExecuteW returned %d", action, r)
	}
}

// Clipboard format constants (winuser.h).
const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
)

// CopyToClipboard writes text to the Windows clipboard as UTF-16
// (CF_UNICODETEXT). The Win32 contract is:
//  1. OpenClipboard(NULL) — null hwnd is fine for short-lived calls.
//  2. EmptyClipboard.
//  3. GlobalAlloc(GMEM_MOVEABLE, size) → hMem.
//  4. GlobalLock(hMem) → pointer; copy UTF-16 bytes; GlobalUnlock.
//  5. SetClipboardData(CF_UNICODETEXT, hMem). Ownership of hMem
//     transfers to the OS on success — do NOT GlobalFree it.
//  6. CloseClipboard.
//
// On any failure between Alloc and SetClipboardData we GlobalFree the
// buffer ourselves to avoid leaking the allocation.
func CopyToClipboard(text string) error {
	utf16 := windows.StringToUTF16(strings.TrimRight(text, "\r\n"))
	size := uintptr(len(utf16) * 2) // UTF-16 unit is 2 bytes

	openR, _, _ := procOpenClipboard.Call(0)
	if openR == 0 {
		return errors.New("clipboard: OpenClipboard failed")
	}
	defer procCloseClipboard.Call()
	_, _, _ = procEmptyClipboard.Call()

	allocR, _, _ := procGlobalAlloc.Call(uintptr(gmemMoveable), size)
	if allocR == 0 {
		return errors.New("clipboard: GlobalAlloc failed")
	}
	freeOnFail := allocR

	lockR, _, _ := procGlobalLock.Call(allocR)
	if lockR == 0 {
		_, _, _ = procGlobalFree.Call(freeOnFail)
		return errors.New("clipboard: GlobalLock failed")
	}
	// Copy the UTF-16 bytes into the OS-owned buffer. We forward
	// both pointers through syscall.SyscallN to RtlMoveMemory; this
	// is the standard Win32 pattern for "move bytes into a buffer
	// returned by GlobalLock" and keeps the uintptr→Pointer
	// conversion inside the syscall arg list (where go vet does not
	// flag it).
	srcPtr := uintptr(unsafe.Pointer(&utf16[0]))
	_, _, _ = procRtlMoveMemory.Call(lockR, srcPtr, size)
	_, _, _ = procGlobalUnlock.Call(allocR)

	setR, _, errno := procSetClipboardData.Call(uintptr(cfUnicodeText), allocR)
	if setR == 0 {
		_, _, _ = procGlobalFree.Call(freeOnFail)
		if e, ok := errno.(syscall.Errno); ok && e != 0 {
			return fmt.Errorf("clipboard: SetClipboardData: %w", e)
		}
		return errors.New("clipboard: SetClipboardData failed")
	}
	return nil
}
