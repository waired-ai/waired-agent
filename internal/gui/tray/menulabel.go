package tray

import "strings"

// menulabel.go answers one question: what does the OS do to a menu label
// before it draws it.
//
// All three backends take a plain string, and two of the three read a
// character in it as markup rather than as text:
//
//   - **Windows.** systray sets the item with MFT_STRING, and Win32 menus
//     treat `&` as the mnemonic prefix (`&File` draws as F̲ile). So a label
//     carrying an ampersand loses it: `Privacy & safety…` drew as
//     `Privacy  safety…` on pc-dell-premium, in a packaged 0.0.3-rc4 tray
//     and in a build of main alike (waired-agent#1096). `&&` draws one `&`.
//   - **Linux.** The dbusmenu spec says of `label`: "two consecutive
//     underscore characters `__` are displayed as a single underscore, any
//     remaining underscore characters are not displayed at all, the first
//     of those remaining underscore characters (unless it is the last
//     character in the string) indicates that the following character is
//     the access key." `&` is ordinary text there.
//   - **macOS.** NSMenuItem.title is drawn verbatim; AppKit has no mnemonic
//     syntax. Nothing to escape.
//
// So the escape is per-OS in both directions: doubling `&` on Linux or
// macOS would put a literal `&&` on the screen.
//
// Underscores are deliberately NOT escaped — see escapeMenuLabel.

// escapeMenuLabel returns s with the characters this OS's menu renderer
// would eat written so that they survive.
//
// goos is a parameter rather than a build tag because the decision is a
// table, and a table is worth testing on all three values from one place
// (the shape internal/setup's initStateDirMode established).
//
// # Why underscores are left alone
//
// The spec above bites a lot of labels: ollama quantisation tags
// (`qwen3.6:35b-a3b-q4_K_M`, which the Model row shows verbatim and the
// peer rows fall back to), the literal `ANTHROPIC_BASE_URL` in the
// Claude row, an email local-part like `first_last@`, a home directory
// like `/home/dev_user/…` in the OpenCode/OpenClaw config rows, and
// whatever an engine put in LastError. Escaping `_` to `__` is the
// spec-correct fix and it
// is correct on Plasma, whose libdbusmenu-qt implements the rule in full
// (swapMnemonicChar: `__` → `_`, first `_` → the Qt mnemonic `&`).
//
// It does not work on GNOME, which is what this fleet's Linux host runs.
// gnome-shell-extension-appindicator's dbusMenu.js does
//
//	label.replace(/_([^_])/, '$1')
//
// with no `g` flag: it strips the FIRST single underscore and never
// collapses `__`. Run against the installed extension's own regex in gjs
// on that host:
//
//	"…q4_K_M"    -> "…q4K_M"      (today)
//	"…q4__K__M"  -> "…q4_K__M"    (escaped)
//
// Both are wrong, and no single string is right on both renderers at once:
// on GNOME the escape only survives for a label with at most one
// underscore. So this doubles nothing on Linux and the underscore case is
// tracked separately, where a fix has to be either upstream or a decision
// to keep underscores out of labels.
func escapeMenuLabel(goos, s string) string {
	if goos != "windows" {
		return s
	}
	return strings.ReplaceAll(s, "&", "&&")
}
