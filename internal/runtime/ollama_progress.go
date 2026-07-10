package runtime

// OllamaInstallProgress is one human-facing update during an Ollama
// install. Stage transitions and notices carry only Message; while an
// archive is streaming down, the download stages additionally emit
// byte-level updates (Completed and/or Total set) so renderers can draw
// a live progress bar instead of sitting silent for the whole
// multi-hundred-MB transfer. Shared by the Linux bundled-tarball
// installer (ollama_install.go) and the macOS Ollama.app flow
// (cmd/waired/runtimes_install_darwin.go, #615) — hence no build tag.
type OllamaInstallProgress struct {
	Stage       string // linux: "download" | "download-rocm" | "extract" | "activate"; darwin: "download" | "unzip" | "install"
	Message     string
	Completed   int64 // byte updates: bytes received so far
	Total       int64 // byte updates: Content-Length; -1 when the server didn't send one
	BytesPerSec int64 // byte updates: smoothed transfer rate; -1 before the first sample
}

// ByteProgress adapts an install-level progress callback into the
// (completed, total, bytesPerSec) shape download.Fetch streams, stamping
// the download stage the bytes belong to.
func ByteProgress(progress func(OllamaInstallProgress), stage string) func(completed, total, bytesPerSec int64) {
	return func(completed, total, bytesPerSec int64) {
		progress(OllamaInstallProgress{Stage: stage, Completed: completed, Total: total, BytesPerSec: bytesPerSec})
	}
}
