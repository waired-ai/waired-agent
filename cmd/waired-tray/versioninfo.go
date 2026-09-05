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
// It is generated from versioninfo.json by `make versioninfo`, which is what
// dist-windows-installer runs; there is no //go:generate line here because
// the generator's version pin lives once, in the Makefile, where Renovate
// can see it (waired-agent#1209).
//
// The committed copy carries placeholder versions (0.0.0.0 / "0.0.0-dev").
// The release build regenerates it with the resolved release version, so
// what ships reports its own build and a local `go build` still gets the
// product names. scripts/install/windows_versioninfo_test.go pins both.
//
// Verified with the Win32 version API (GetFileVersionInfo / VerQueryValue,
// the path Explorer and Task Manager use) reporting FileDescription="Waired".
// waired#810 also recorded that .NET's System.Diagnostics.FileVersionInfo
// can read the same resource as empty; installtest-windows.ps1 now reads it
// both ways on every run rather than leaving that as a claim.
