//go:build darwin && cgo

package tray

/*
#cgo darwin LDFLAGS: -framework Cocoa
void wairedSetAccessoryActivationPolicy(void);
*/
import "C"

// setActivationPolicyAccessory hides the Dock icon / Cmd-Tab entry so
// the tray presents only its menu-bar status item. It must be called on
// the program's main goroutine (the same thread fyne.io/systray's run
// loop uses) before systray.Run starts the AppKit run loop, so the Dock
// icon never momentarily appears. The Objective-C implementation lives
// in activation_darwin.m (cgo compiles .m files with the ObjC compiler).
func setActivationPolicyAccessory() {
	C.wairedSetAccessoryActivationPolicy()
}
