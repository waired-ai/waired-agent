// Objective-C implementation for setActivationPolicyAccessory (see
// activation_darwin.go). Gated to darwin by the _darwin filename suffix;
// cgo only compiles .m files when CGO is enabled, matching the
// `darwin && cgo` build tag on the Go side.

#import <Cocoa/Cocoa.h>

void wairedSetAccessoryActivationPolicy(void) {
	// Run as a menu-bar-only accessory: no Dock icon, no Cmd-Tab entry,
	// no app-switcher presence. fyne.io/systray's darwin backend creates
	// the NSStatusItem but never sets an activation policy, so a bare
	// (non-.app) ad-hoc binary would otherwise launch as a regular
	// foreground application complete with a Dock icon. Touching
	// sharedApplication first guarantees NSApp exists before we set the
	// policy on it.
	[NSApplication sharedApplication];
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}
