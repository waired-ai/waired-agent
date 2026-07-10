//go:build windows

package notification

import (
	"errors"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsNotifier uses Shell_NotifyIcon balloon tips. A hidden helper
// window owns the tray-icon registration so we can call NIM_ADD /
// NIM_MODIFY without depending on fyne.io/systray exposing its HWND.
// The icon is the default information icon at WM_USER+1 message id —
// it is never made visible (the balloon tip is the only user-facing
// output).
//
// Lazy init: the helper window + icon registration is built on first
// Notify() and reused across calls. Cleanup happens at process exit
// (Windows discards the NIM_ADD record automatically).
//
// Why balloon tips instead of WinRT toast: balloon needs no app
// manifest and works for unsigned binaries. WinRT toast requires a
// registered AppUserModelID with a Start Menu shortcut, which is a
// reasonable goal but out of scope for the initial Windows tray port.
type windowsNotifier struct {
	once sync.Once
	hwnd windows.Handle
	uid  uint32
	err  error
}

func newNotifier() Notifier { return &windowsNotifier{} }

const (
	// Shell_NotifyIcon constants from shellapi.h.
	nim_add      = 0x00000000
	nim_modify   = 0x00000001
	nim_delete   = 0x00000002
	nif_message  = 0x00000001
	nif_icon     = 0x00000002
	nif_tip      = 0x00000004
	nif_state    = 0x00000008
	nif_info     = 0x00000010
	niif_info    = 0x00000001
	niif_warning = 0x00000002
	niif_error   = 0x00000003

	// CreateWindowEx style — invisible, no chrome.
	ws_overlapped = 0x00000000

	// Pre-defined system icon resource IDs.
	idi_information = 32516
)

// notifyIconData mirrors NOTIFYICONDATAW (size matches the V2/V3
// layout that ships on every supported Windows: Windows 10+).
type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIcW  = shell32.NewProc("Shell_NotifyIconW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procRegisterClassEx = user32.NewProc("RegisterClassExW")
	procLoadIcon        = user32.NewProc("LoadIconW")
)

// init helper: register a window class + hidden window the first time
// Notify is called. Best-effort: any failure leaves n.err set and
// future Notify calls return nil (no toast, but no error).
func (n *windowsNotifier) ensure() {
	n.once.Do(func() {
		className := windows.StringToUTF16Ptr("WairedNotifyHelper")

		var wc struct {
			Size       uint32
			Style      uint32
			WndProc    uintptr
			ClsExtra   int32
			WndExtra   int32
			Instance   windows.Handle
			Icon       windows.Handle
			Cursor     windows.Handle
			Background windows.Handle
			MenuName   *uint16
			ClassName  *uint16
			IconSm     windows.Handle
		}
		wc.Size = uint32(unsafe.Sizeof(wc))
		wc.WndProc = procDefWindowProc.Addr()
		wc.ClassName = className
		// Ignore the return — RegisterClassEx may fail with
		// ERROR_CLASS_ALREADY_EXISTS on repeated process restarts of a
		// long-running tray, which is harmless.
		_, _, _ = procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))

		hwnd, _, _ := procCreateWindowEx.Call(
			0,                                  // dwExStyle
			uintptr(unsafe.Pointer(className)), // lpClassName
			0,                                  // lpWindowName
			ws_overlapped,                      // dwStyle
			0, 0, 0, 0,                         // x, y, width, height
			0, 0, 0, 0, // hWndParent, hMenu, hInstance, lpParam
		)
		if hwnd == 0 {
			n.err = errors.New("notification: CreateWindowEx failed")
			return
		}
		n.hwnd = windows.Handle(hwnd)
		n.uid = 1
	})
}

func (n *windowsNotifier) Notify(title, body string, level Level) error {
	if title == "" {
		return errors.New("notification: empty title")
	}
	n.ensure()
	if n.err != nil || n.hwnd == 0 {
		// init failed; silently skip rather than propagate
		return nil
	}

	hIcon, _, _ := procLoadIcon.Call(0, idi_information)

	var data notifyIconData
	data.CbSize = uint32(unsafe.Sizeof(data))
	data.HWnd = n.hwnd
	data.UID = n.uid
	data.UFlags = nif_info | nif_icon
	data.HIcon = windows.Handle(hIcon)
	copyUTF16(data.SzInfoTitle[:], title)
	copyUTF16(data.SzInfo[:], body)
	switch level {
	case Warning:
		data.DwInfoFlags = niif_warning
	case Error:
		data.DwInfoFlags = niif_error
	default:
		data.DwInfoFlags = niif_info
	}

	// First call adds the icon, subsequent calls modify the balloon.
	r, _, _ := procShellNotifyIcW.Call(uintptr(nim_add), uintptr(unsafe.Pointer(&data)))
	if r == 0 {
		// NIM_ADD failed (icon may already be registered); try modify.
		_, _, _ = procShellNotifyIcW.Call(uintptr(nim_modify), uintptr(unsafe.Pointer(&data)))
	}
	return nil
}

// copyUTF16 writes s into dst[] as a UTF-16 string, truncating to fit
// and leaving a trailing NUL when there is room.
func copyUTF16(dst []uint16, s string) {
	utf16 := windows.StringToUTF16(s)
	n := len(utf16)
	if n > len(dst) {
		n = len(dst)
	}
	copy(dst, utf16[:n])
	if n < len(dst) {
		dst[n] = 0
	}
}
