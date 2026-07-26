// Package browser opens a URL in the user's default web browser and reports
// whether the current session can plausibly show one. It is the single
// cross-platform implementation shared by the CLI (`waired init`,
// `waired codeui open`) and the desktop tray, which previously each carried
// their own per-OS OpenBrowser.
//
// Open is per-OS (browser_{linux,darwin,windows}.go): xdg-open on Linux,
// `open(1)` on macOS, rundll32 url.dll,FileProtocolHandler on Windows.
//
// On the Unixes Open also crosses back over the privilege boundary: the
// installers elevate before running `waired init`, and "the default browser"
// is a property of the DESKTOP user, not of root — macOS resolves it from the
// effective uid's LaunchServices map and Linux from the session's MIME
// database and D-Bus. So whenever the process is root, Open de-escalates to
// the invoking user before launching, and falls back to the direct launch if
// that cannot be done. Callers therefore never choose between a privileged
// and an unprivileged opener; there is one entry point and it does the right
// thing. See desktopuser.go for the argv, which is where all three per-OS
// defects (#181 / #182 / #183) lived.
package browser
