package main

// Windows VERSIONINFO resource. Without this, waired.exe carries no version
// resource: Explorer's Properties dialog and Task Manager's "Details" column
// show nothing for it, and every CodeIntegrity event and third-party tool
// that quotes a file version quotes 0.0.0.0 (waired-agent#1209). The
// resource makes them show "Waired (CLI)" — the same name the Start Menu
// entry uses — and the version this build actually is.
//
// The committed resource_windows_amd64.syso is picked up automatically by
// the Go toolchain, but ONLY on windows/amd64 (the _windows_amd64 filename
// suffix is a build constraint), so linux/darwin builds are untouched. It is
// generated from versioninfo.json by `make versioninfo`, which is what
// dist-windows-installer runs; the generator's version pin lives once, in
// the Makefile, where Renovate can see it.
//
// The committed copy carries placeholder versions (0.0.0.0 / "0.0.0-dev").
// The release build regenerates it with the resolved release version, so
// what ships reports its own build and a local `go build` still gets the
// product names. scripts/install/windows_versioninfo_test.go pins both.
