//go:build windows

package trust

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// caSubjectMatch identifies the retired proxy CA in the Windows Root store for
// `certutil -delstore`. Sourced from the shared trust.CACommonName so the
// Windows and macOS untrust paths cannot drift from each other.
const caSubjectMatch = CACommonName

// envRegPath is the machine-wide environment block. Values written here are
// inherited by every process started after the WM_SETTINGCHANGE broadcast (and
// by every new logon session). Writing it requires elevation — on the agent
// path the caller is LocalSystem.
const envRegPath = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// nodeExtraCAEnv is the Node.js variable that points at an extra CA bundle.
// Claude Code (a Node app) ignores the OS trust store, so the minted leaves are
// only trusted once this points at the proxy CA PEM.
const nodeExtraCAEnv = "NODE_EXTRA_CA_CERTS"

// cryptNotFound is the locale-independent CRYPT_E_NOT_FOUND code certutil prints
// (alongside localized text) when -delstore matches nothing. Treated as success
// — the CA is already absent. We match the hex code because certutil's prose is
// localized (e.g. Japanese on a ja-JP host).
const cryptNotFound = "0x80092004"

// UninstallCA removes the proxy CA from the LocalMachine Root store. A missing
// entry (CRYPT_E_NOT_FOUND) is not an error.
func UninstallCA() error {
	out, err := runCertutil("-delstore", "Root", caSubjectMatch)
	if err != nil {
		if strings.Contains(out, cryptNotFound) {
			return nil
		}
		return fmt.Errorf("trust: certutil -delstore Root: %w: %s", err, out)
	}
	return nil
}

// UninstallNodeExtraCA clears the machine-wide NODE_EXTRA_CA_CERTS. A missing
// value is not an error.
func UninstallNodeExtraCA() error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, envRegPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("trust: open env registry: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(nodeExtraCAEnv); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("trust: delete %s: %w", nodeExtraCAEnv, err)
	}
	broadcastEnvChange()
	return nil
}

// runCertutil runs certutil with args and returns its combined output. certutil
// ships in System32 on every supported Windows.
func runCertutil(args ...string) (string, error) {
	bin, err := exec.LookPath("certutil")
	if err != nil {
		return "", fmt.Errorf("certutil not found: %w", err)
	}
	var buf bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	return buf.String(), runErr
}

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procSendMessageTimO = user32.NewProc("SendMessageTimeoutW")
)

// broadcastEnvChange notifies running processes that the environment block
// changed. Best-effort: failures are non-fatal because newly-spawned processes
// (a fresh Claude Code) inherit the value regardless; the broadcast only
// refreshes already-running shells (Explorer and its children).
func broadcastEnvChange() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var result uintptr
	_, _, _ = procSendMessageTimO.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(env)),
		uintptr(smtoAbortIfHung),
		uintptr(5000),
		uintptr(unsafe.Pointer(&result)),
	)
}
