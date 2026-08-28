package tray

import (
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// menulabel.go answers one question: what does the thing drawing this menu do
// to a label before it shows it.
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
// Linux needs one more axis, because the renderers disagree with each other
// as well as with us — see escapeMenuLabel.

// escapeMenuLabel returns s with the characters this session's menu renderer
// would eat written so that they survive.
//
// goos and dialect are parameters rather than build tags and probes because
// the decision is a table, and a table is worth testing on every value from
// one place (the shape internal/setup's initStateDirMode established). dialect
// is read only for linux; the other two have no underscore rule.
//
// # Underscores, and why there are two answers on Linux
//
// The spec quoted above bites a lot of labels. The Recent activity rows carry
// the router's own wire tags (`engine_not_ready`, `share_off`), a device id is
// `dev_28ab996e`, a peer whose agent predates active_model falls back to the
// engine tag `qwen3.6:35b-a3b-q4_K_M`, the Claude row names
// `ANTHROPIC_BASE_URL`, an email local part is `first_last@`, an OpenCode
// config path holds a home directory, and an engine's LastError holds
// whatever it holds. Two or more underscores in one label is the common case,
// not the exception.
//
// Escaping `_` to `__` is what the spec asks for, and it is exactly right for
// Plasma (plasma-workspace vendors libdbusmenu-qt's swapMnemonicChar
// unchanged), for libdbusmenu-gtk and therefore for xfce4-panel, Waybar and
// snixembed. It is also what the toolkits do for everyone else: Chromium's
// ConvertAmpersandsTo doubles underscores for every Electron tray, and
// libdbusmenu-gtk's parser.c doubles them on the writing side — which is how
// getlantern/systray got this right for free, and what fyne.io/systray
// dropped when it replaced cgo and libappindicator with its own D-Bus code.
//
// It is NOT right on GNOME, whose only SNI host is
// gnome-shell-extension-appindicator. Its dbusMenu.js does
//
//	propertyGet('label').replace(/_([^_])/, '$1')
//
// with no `g` flag, so it deletes exactly one underscore — the first one
// followed by a non-underscore — and never collapses `__`. Measured by
// running the installed extension's own regex in gjs on the fleet's GNOME
// host (Shell 50.1, ubuntu-appindicators):
//
//	                      raw          "_"->"__"      escapeForGnomeShell
//	"…q4_K_M"             q4K_M        q4_K__M        …q4_K_M
//	"ANTHROPIC_BASE_URL"  ANTHROPIC…   …BASE__URL     ANTHROPIC_BASE_URL
//	"first_last@corp"     firstlast@   first_last@    first_last@corp
//	"abc_"                abc_         abc__          abc_
//
// So there is no one string that both renderers draw correctly — but there is
// one for each of them, and which one is drawing is knowable at run time
// (trayhost.MenuLabels reads who owns org.kde.StatusNotifierWatcher). That is
// the ruling this table implements: emit spec-correct markup, except to the
// one renderer measured to misread it (waired-agent#1100).
func escapeMenuLabel(goos string, dialect trayhost.MenuDialect, s string) string {
	switch goos {
	case "windows":
		return strings.ReplaceAll(s, "&", "&&")
	case "linux":
		if dialect == trayhost.MenuDialectGnomeShell {
			return escapeForGnomeShell(s)
		}
		return strings.ReplaceAll(s, "_", "__")
	default:
		// darwin, and any OS whose menus this table has never been
		// checked against. The escape ADDS markup, so the safe answer for
		// an unknown renderer is to add none: a stray `&&` on screen is a
		// defect, a stray `&` is what the string said.
		return s
	}
}

// escapeForGnomeShell writes s the way gnome-shell-extension-appindicator has
// to receive it to draw s.
//
// The extension's regex deletes the first underscore that is followed by a
// non-underscore, and nothing else — so exactly one underscore goes missing,
// always from the first run of them. Adding one underscore to that run feeds
// the regex the character it is going to eat and leaves the rest of the string
// alone; every later run passes through untouched, which is why this is not
// `_`->`__` on a leash.
//
// A run at the very end of the string has no following character, so the regex
// never matches it and nothing is eaten: `abc_` already draws as `abc_` there,
// and doubling it would be the defect rather than the fix. Since the first run
// is the only one that can match, a trailing first run means the whole label
// is already safe.
//
// Byte indexing is correct because `_` is ASCII and every byte of a multi-byte
// UTF-8 rune is >= 0x80.
func escapeForGnomeShell(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			continue
		}
		j := i
		for j < len(s) && s[j] == '_' {
			j++
		}
		if j == len(s) {
			return s // trailing run: the renderer eats nothing
		}
		return s[:i] + "_" + s[i:]
	}
	return s
}
