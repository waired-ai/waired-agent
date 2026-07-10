// Package installscripts embeds the OS installer scripts that live in
// scripts/install/ so compiled binaries (e.g. `waired runtimes install
// ollama`) can run them without the source tree present on the target
// machine. The scripts stay in their canonical location — this package
// only re-exports their bytes via go:embed, keeping a single source of
// truth.
package installscripts

import _ "embed"

// OllamaWindowsPS1 is the verbatim contents of ollama-windows.ps1, the
// Windows Ollama installer (downloads the official ZIP + optional ROCm
// overlay into %ProgramFiles%\Ollama). Consumed by the Windows build of
// `waired runtimes install ollama`.
//
//go:embed ollama-windows.ps1
var OllamaWindowsPS1 []byte
