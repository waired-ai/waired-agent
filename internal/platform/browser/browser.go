// Package browser opens a URL in the user's default web browser and reports
// whether the current session can plausibly show one. It is the single
// cross-platform implementation shared by the CLI (`waired codeui open`) and
// the desktop tray, which previously each carried their own per-OS OpenBrowser.
//
// Open is per-OS (browser_{linux,darwin,windows}.go): xdg-open on Linux,
// `open(1)` on macOS, rundll32 url.dll,FileProtocolHandler on Windows.
package browser
