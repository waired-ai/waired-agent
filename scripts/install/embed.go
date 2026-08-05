// Package installscripts embeds the OS installer scripts that live in
// scripts/install/ so compiled binaries (e.g. `waired runtimes install
// ollama`) can run them without the source tree present on the target
// machine. The scripts stay in their canonical location — this package
// only re-exports their bytes via go:embed, keeping a single source of
// truth.
package installscripts

// Nothing is embedded here today. ollama-windows.ps1 was, until #493 moved
// the Windows engine install into the shared Go installer alongside Linux
// and macOS; the package stays because encoding_test.go's mirror check is
// what keeps waired-agent-windows.ps1 and its install.ps1 twin from
// drifting apart in code-page handling.
