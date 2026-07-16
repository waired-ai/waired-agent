package main

// Windows VERSIONINFO resource. Without this, waired-tray.exe carries no
// version resource, so Task Manager's "Details" column and Explorer's file
// Properties show the bare binary name "waired-tray". The resource makes
// them show the user-facing product name "Waired" instead (waired#810) —
// the process/binary name stays waired-tray.exe (the CLI owns waired.exe).
//
// The committed resource_windows_amd64.syso is picked up automatically by
// the Go toolchain, but ONLY on windows/amd64 (the _windows_amd64 filename
// suffix is a build constraint), so linux/darwin tray builds are untouched.
// It is generated from versioninfo.json; regenerate after editing that file
// with `make versioninfo` or `go generate ./cmd/waired-tray`.
//
// Verified with the Win32 version API (GetFileVersionInfo / VerQueryValue,
// the path Explorer and Task Manager use) reporting FileDescription="Waired";
// note .NET's System.Diagnostics.FileVersionInfo can read this same resource
// as empty — a known wrapper quirk, not a defect in the resource.
//
//go:generate go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0 -64 -o resource_windows_amd64.syso versioninfo.json
