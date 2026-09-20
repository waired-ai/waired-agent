package runtime

import (
	"log/slog"
	"strings"
	"time"
)

// A model load that runs out of memory is a different fact from an engine
// that broke, and until now the product could not tell them apart.
//
// On the reference host of waired-agent#1443 the runner dies and ollama
// answers `llama-server process has terminated: exit status 0xe06d7363`.
// That matches engineDeadMarkers, which is checked first, so the product
// demoted the engine and restarted it — straight back into the same load,
// which failed the same way. The remedy is not a restart. It is a smaller
// model, and this file is how the two are told apart.
//
// The evidence is in the engine log rather than the reply, because the
// reply is the same sentence in both cases. Two shapes count:
//
//  1. The runner said an allocation failed. Unambiguous.
//  2. The runner said nothing at all, and the last thing it printed was the
//     step at which the memory has to become resident. That is the #1443
//     shape: on that host every allocation SUCCEEDS — the device total
//     reaches 85,074 MiB — and the process dies at
//     `warming up the model with an empty run`, the first moment the
//     reserved memory is touched.

// loadMemoryAllocMarkers are substrings of an ENGINE LOG that name memory as
// the reason a runner died.
//
// Narrow on the same terms as engineOOMMarkers: a marker earns its place
// from an observed run naming the host and model that produced it, or from
// the upstream source line that emits it, quoted exactly. A guessed marker
// never matches and nobody finds out.
var loadMemoryAllocMarkers = []string{
	// ggml/src/ggml-vulkan/ggml-vulkan.cpp, ggml_vk_create_buffer_check:
	// printed before it rethrows. Absent from every #1443 log, which is
	// how that measurement established the failure is not an allocation.
	"ggml_vulkan: Memory allocation of size",
	// The Vulkan-Hpp exception the same path throws when nothing catches
	// it first.
	"vk::OutOfDeviceMemoryError",
	// ggml emits this as GGML_CUDA_NAME " error: %s", and GGML_CUDA_NAME is
	// "ROCm" under GGML_USE_HIP — the same reason engineOOMMarkers carries
	// both spellings.
	"CUDA error: out of memory",
	"ROCm error: out of memory",
	// The host-side allocator giving up. Seen from llama.cpp when a host
	// buffer cannot be taken.
	"std::bad_alloc",
	// ggml asserting on a context whose buffer could not be allocated.
	// Observed on the reference host on 2026-09-20 while deliberately
	// bounding the engine's committed memory with a Job Object: the
	// allocation returns NULL and llama.cpp aborts here rather than
	// reporting a failure, so this assert is the only thing it says about
	// running out. Windows renders that abort as exit status 0xc0000409,
	// which reads as a stack buffer overrun and is not one.
	"GGML_ASSERT(ctx->mem_buffer != NULL) failed",
}

// loadMemoryResidencySteps are the runner's own words for the steps at which
// reserved memory has to become resident. A runner whose LAST word is one of
// these did not fail to ask for memory; it failed to keep it.
//
// Both are observed: the warm-up line is the last line of all five failing
// loads measured on the reference host on 2026-09-20, and the tensor-loading
// line is where waired-ai/waired#1427's DeepSeek run died.
var loadMemoryResidencySteps = []string{
	"warming up the model with an empty run",
	"load_tensors:",
}

// loadMemoryFacts is what one failed load could be read from.
type loadMemoryFacts struct {
	// Body is the engine's error reply, if there was one.
	Body string
	// LogTail is the end of the engine log.
	LogTail string
	// TailStale is EngineLogTailIsStale(LogTail): the log is at its cap and
	// its last bytes are from the engine's first minutes, not from the load
	// that just failed. Shape 2 is a claim about the LAST thing printed, so
	// it cannot be made about text that is not the last thing printed.
	TailStale bool
}

// loadMemoryFailure reports whether a load failed for want of memory, and
// the sentence to put in front of an operator when it did.
//
// Untagged and total: every input decides, so the table test is the whole
// specification.
func loadMemoryFailure(f loadMemoryFacts) (bool, string) {
	for _, m := range loadMemoryAllocMarkers {
		if strings.Contains(f.LogTail, m) {
			return true, "the engine ran out of memory loading the model; it reported " + m
		}
	}
	if f.TailStale {
		return false, ""
	}
	if !runnerIsGone(f.Body) {
		return false, ""
	}
	last := lastRunnerLine(f.LogTail)
	if last == "" {
		return false, ""
	}
	for _, step := range loadMemoryResidencySteps {
		if strings.Contains(last, step) {
			return true, "this computer ran out of memory putting the model in memory; " +
				"the engine stopped without reporting a failed allocation"
		}
	}
	return false, ""
}

// runnerIsGone reports whether the reply says the model runner died, which
// shape 2 needs before it reads anything into where the log stops. A runner
// that is still alive has not stopped anywhere.
//
// It takes engineDeadMarkers plus the two ways a supervisor reports a
// process the kernel killed: Go renders a signalled exit as "signal: killed"
// and Windows reports the code numerically.
func runnerIsGone(body string) bool {
	for _, m := range engineDeadMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return strings.Contains(body, "signal: killed")
}

// lastRunnerLine returns the last line the RUNNER wrote, skipping the lines
// ollama's own supervisor appends afterwards.
//
// It matters because the runner's last word is the whole of shape 2, and
// ollama always writes after it: a real failing tail ends with three
// `time=… level=INFO source=llama_server.go…` lines and a `Load failed`,
// below the runner's `warming up the model…`. Structured ollama lines are
// recognised by the `time=` stamp they all begin with, which the runner
// never writes.
//
// The first line of the tail is dropped: a tail is cut at a byte offset, so
// its first line is usually half a line.
func lastRunnerLine(tail string) string {
	lines := strings.Split(tail, "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "time=") {
			continue
		}
		return s
	}
	return ""
}

// LoadMemoryFailure is one load that failed for want of memory, as the
// adapter classified it.
type LoadMemoryFailure struct {
	// Reason is the sentence to put in front of an operator. It says what
	// happened, not what the engine printed.
	Reason string
	// Detail is the engine's own first line, kept so a log or a bug report
	// carries the words the engine actually used.
	Detail string
	// At is when this was classified.
	At time.Time
}

// classifyLoadMemory asks whether an engine reply that says the runner is
// gone is really a load that ran out of memory.
//
// The engine log is read here rather than by the caller because the tail's
// staleness is part of the answer, and only the adapter knows where its log
// is. A missing log (LogDir unset, an adopted engine) yields an empty tail,
// which decides "not memory" — the positional shape cannot be claimed about
// text nobody has.
func (a *OllamaAdapter) classifyLoadMemory(body string) (LoadMemoryFailure, bool) {
	tail := tailEngineLog(a.engineLogPath(), engineLogTailMaxBytes)
	ok, reason := loadMemoryFailure(loadMemoryFacts{
		Body:      body,
		LogTail:   tail,
		TailStale: EngineLogTailIsStale(tail),
	})
	if !ok {
		return LoadMemoryFailure{}, false
	}
	return LoadMemoryFailure{Reason: reason, Detail: firstLine([]byte(body)), At: time.Now()}, true
}

// reportLoadMemoryFailure fires OnLoadMemoryFailure at most once per
// unhealthyDebounce, and never demotes.
//
// Debounced on its own clock rather than markUnhealthy's or
// reportFitFailure's: a load that failed for memory leaves nothing resident,
// so every request that arrives next pays a cold reload and fails the same
// way. That burst is one fact about one build.
func (a *OllamaAdapter) reportLoadMemoryFailure(f LoadMemoryFailure) {
	a.mu.Lock()
	if !a.lastLoadMemFailure.IsZero() && time.Since(a.lastLoadMemFailure) < unhealthyDebounce {
		a.mu.Unlock()
		return
	}
	a.lastLoadMemFailure = time.Now()
	fn := a.cfg.OnLoadMemoryFailure
	a.mu.Unlock()

	slog.Warn("ollama: this computer ran out of memory loading the model",
		"reason", f.Reason, "engine_said", f.Detail)
	if fn != nil {
		go fn(f)
	}
}
