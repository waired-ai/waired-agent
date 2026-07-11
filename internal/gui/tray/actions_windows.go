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
	return shellExecuteRunAs(ctx, exe, args)
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
	return shellExecuteRunAs(ctx, exe, args)
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
	return shellExecuteRunAs(ctx, exe, args)
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
	return shellExecuteRunAs(ctx, exe, []string{"update", "--yes"})
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
// failure otherwise. We deliberately do not wait for the elevated
// process to exit (parent → elevated stdout is not pipe-able); the
// tray observes completion by polling /v1/identity.
func shellExecuteRunAs(_ context.Context, exe string, args []string) error {
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
		return errors.New("login: UAC consent declined")
	case 2:
		return fmt.Errorf("login: %s not found", exe)
	case 3:
		return errors.New("login: path not found")
	case 8:
		return errors.New("login: out of memory")
	default:
		return fmt.Errorf("login: ShellExecuteW returned %d", r)
	}
}

// OpenBrowser launches the URL with the user's default handler.
// rundll32 url.dll,FileProtocolHandler is the safe form: it does not
// require COM init and is the same call `start <url>` makes
// internally. We deliberately do NOT shell out via `cmd /c start` —
// that would inherit the tray's stdin and risk a window flash.
func OpenBrowser(url string) error {
	if url == "" {
		return errors.New("OpenBrowser: empty url")
	}
	rundll, err := windows.UTF16PtrFromString("rundll32.exe")
	if err != nil {
		return err
	}
	args, err := windows.UTF16PtrFromString(`rundll32.exe url.dll,FileProtocolHandler ` + url)
	if err != nil {
		return err
	}
	var startupInfo windows.StartupInfo
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))
	var procInfo windows.ProcessInformation
	if err := windows.CreateProcess(
		rundll, args, nil, nil, false,
		windows.CREATE_NO_WINDOW, // no console flash
		nil, nil,
		&startupInfo, &procInfo,
	); err != nil {
		return fmt.Errorf("OpenBrowser: CreateProcess: %w", err)
	}
	_ = windows.CloseHandle(procInfo.Process)
	_ = windows.CloseHandle(procInfo.Thread)
	return nil
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
