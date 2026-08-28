package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/catalog"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The two sentences the give-up latch records, and the one place they are
// built (waired-agent#1069).
//
// They were four fmt.Sprintf calls — one per engine per failure shape —
// and the diagnosis that names the actual cause reached none of them.
const (
	giveUpStartHeadline = "engine failed to start %d times within %s; automatic restart disabled" +
		" — see the engine log, then `waired inference engine start` (or switch model) to retry"
	giveUpCrashHeadline = "engine crashed %d times within %s; automatic restart disabled" +
		" — see the engine log, then `waired inference engine start` (or switch model) to retry"
)

// engineGiveUpMessage composes what LatchFailed records: the diagnosis
// first, then the give-up sentence, then the raw failure detail the
// adapter handed us (which already carries the engine-log tail — see
// engineExitError).
//
// The diagnosis goes FIRST and the latch carries it, rather than being
// prepended afterwards by whoever noticed the cause. Before this, the two
// writers were SetStartFailureReason and LatchFailed, in that conceptual
// order and no enforced one: LatchFailed assigns a whole Health value, so
// landing second it erased the named cause, and landing first it let the
// bootstrap's prepend duplicate it. Which one won was a goroutine race —
// OnStartFailed fires from runStart's defer on a fresh goroutine while the
// bootstrap is still unwinding out of EnsureRunning (waired-agent#1069).
//
// One writer means one order. It also means the sentence reaches
// giveUpErr, which is the copy with the right lifetime: Stop() clears
// Health.LastErr with no giveUp guard, so the wizard's engine row
// (setupEngineHealth) reads giveUpErr and could never see a diagnosis
// that only ever lived in Health.
//
// An empty diagnosis produces exactly what the four Sprintf calls did, so
// an engine failing for a reason nothing recognises is worded as before.
func engineGiveUpMessage(diagnosis, headline, detail string) string {
	msg := headline
	if diagnosis != "" {
		msg = diagnosis + "\n" + headline
	}
	if detail == "" {
		return msg
	}
	return msg + "\n" + detail
}

// engineStartGiveUp and engineCrashGiveUp are the two shapes, rendered.
func engineStartGiveUp(diagnosis string, n int, window time.Duration, detail string) string {
	return engineGiveUpMessage(diagnosis, fmt.Sprintf(giveUpStartHeadline, n, window), detail)
}

func engineCrashGiveUp(diagnosis string, n int, window time.Duration, detail string) string {
	return engineGiveUpMessage(diagnosis, fmt.Sprintf(giveUpCrashHeadline, n, window), detail)
}

// diagnoseEngineFailure names the cause of a failed start or crash from
// the detail the adapter reported, for the engine that produced it.
//
// The detail is the right input and no file needs re-reading: every
// constructor that builds it — startupExitError, servingExitError, the
// ollama readiness-deadline branch, markUnhealthy — folds the tail of
// engine.log into the message before the callback fires. Scoping it to
// the last spawn is still correct and still cheap: on vLLM the banner is
// in the tail, and on ollama the log is truncated per spawn so the whole
// thing IS one spawn (see LastEngineLogSpawn).
//
// Silent on anything it does not recognise, in both arms. A wrong hint on
// a start-up failure is worse than none: it sends someone to fix
// something that is not broken, and the sentence looks exactly as
// authoritative as a right one.
func diagnoseEngineFailure(engine, detail, addr string) string {
	spawn := infruntime.LastEngineLogSpawn(detail)
	switch engine {
	case "vllm":
		return vllmStartupDiagnosis(spawn, addr)
	case "ollama":
		return ollamaStartupDiagnosis(spawn, addr)
	}
	return ""
}

// enginePortBusyDiagnosis is the sentence both engines use when something
// else owns the port they were told to bind. One builder so the two cannot
// drift: the wording is quoted verbatim in docs-site's troubleshooting
// page, and a second copy is how a fix to one of them silently stops
// matching the docs.
//
// It names the address the way the local gateway's own bind failure does.
// "address already in use" with no number is the least useful thing to
// hand someone.
func enginePortBusyDiagnosis(addr, setting string) string {
	return fmt.Sprintf("another program is already listening on %s,"+
		" the port the inference engine was told to use"+
		" — set %s in agent.json to a free port", addr, setting)
}

// ollamaStartupDiagnosis is vllmStartupDiagnosis' counterpart: it turns
// ollama's own engine.log text into the one sentence that names the cause
// (waired-agent#1069).
//
// ONE arm, and the shortness is a finding rather than an omission. The bar
// its twin sets is engine text this project has actually captured from a
// named host, and a sweep of the knowledge notes, decision records, test
// fixtures, issues and PR bodies in both repositories cleared only the
// candidate below. What was considered and left out:
//
//   - `Error: $HOME is not defined` (#22, captured on the self-hosted
//     macOS runner). Real, but fixed on two axes since: the LaunchDaemon
//     plist emits HOME, and ChildBaseEnv injects one when the launcher
//     gave none. It could only fire if both regressed, which is a job for
//     a test, not for a sentence shown to a person.
//   - `signal: killed` (the macOS bundle whose signature no longer
//     verifies, #329/#330). Unreachable from here twice over: the kernel
//     kills that process at exec, so it writes nothing to engine.log — and
//     openEngineLog has just rotated the previous spawn's text away — while
//     the exit-status text that DOES carry it cannot separate a signature
//     kill from an OOM kill, which two decision records state outright.
//   - `no Metal device` / `ggml_metal_init`. Exists only in hand-written
//     test fixtures. When the real macOS incident finally had its
//     engine.log captured, the answer was $HOME, and the fixing PR says
//     "Not Metal / tart / OOM" in as many words.
//   - `CUDA error: out of memory` (#1038) and the llama-server segfault
//     (#29). Both real, both a 500 response BODY rather than engine.log,
//     and both already owned — by OnFitFailure and markUnhealthy.
//
// The busy-port arm fires ONLY for a holder that is not an ollama.
// EnsureRunning probes /api/version first: an ollama at the pinned version
// is adopted, and any other ollama is refused with a message that already
// names the port, both versions and inference.ollama_port. What was left
// with nothing to say is a non-ollama holder, which falls through to the
// raw start-up error — the same hole #1026 closed for vLLM.
//
// The Unix text was captured on real hardware for this change, with a
// python listener holding the waired-owned port:
//
//	Error: listen tcp 127.0.0.1:9475: bind: address already in use
//
// Note it names the address, unlike vLLM's Python OSError — which is why
// that arm has to be handed one. addr is still what the config SAYS, which
// is the more authoritative of the two.
//
// The Windows phrasing is the same failure in the OS's own words, and it
// is measured rather than expected (#1085):
//
//	listen tcp 127.0.0.1:9475: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.
//
// ollama binds through Go's net package, so the text a second bind
// produces is the text ollama writes.
// TestOllamaStartupDiagnosis_MatchesThisOSBindError takes that text from
// the running OS rather than from a fixture, so this arm goes red on the
// windows CI leg if a future Windows or Go release rewords it — which is
// the guarantee the line above needs, since a substring that stops
// matching fails silently.
//
// darwin needs no third spelling: it is POSIX EADDRINUSE, the same
// "address already in use" the Unix capture above recorded.
func ollamaStartupDiagnosis(engineLog, addr string) string {
	switch {
	case strings.Contains(engineLog, "address already in use"),
		strings.Contains(engineLog, "Address already in use"),
		strings.Contains(engineLog, "Only one usage of each socket address"):
		return enginePortBusyDiagnosis(addr, "inference.ollama_port")
	}
	return ""
}

// engineFailureDiagnosis is diagnoseEngineFailure with the address this
// provider told the engine to bind, which is the one input the log text
// cannot supply: the busy-port arm matches an OSError that does not name
// an address, and the config is what the engine was TOLD.
func (p *agentInferenceProvider) engineFailureDiagnosis(engine, detail string) string {
	if p == nil {
		return ""
	}
	port := 0
	switch engine {
	case catalog.RuntimeVLLM:
		port = p.cfg.ResolvedVLLMPort()
	case catalog.RuntimeOllama:
		port = p.cfg.ResolvedOllamaPort()
	}
	return diagnoseEngineFailure(engine, detail, fmt.Sprintf("127.0.0.1:%d", port))
}

// refuseEngineBootstrap records why the engine bootstrap declined before it
// could build an adapter, and clearEngineBootstrapRefusal takes it back.
//
// Both are no-ops on a nil provider so the Linux-only bootstrap can call
// them without a guard at every site.
func (p *agentInferenceProvider) refuseEngineBootstrap(reason string) {
	if p == nil || reason == "" {
		return
	}
	p.engineBootstrapRefusal.Store(&reason)
}

func (p *agentInferenceProvider) clearEngineBootstrapRefusal() {
	if p == nil {
		return
	}
	p.engineBootstrapRefusal.Store(nil)
}

// engineBootstrapRefused is the recorded reason, or "".
func (p *agentInferenceProvider) engineBootstrapRefused() string {
	if p == nil {
		return ""
	}
	if r := p.engineBootstrapRefusal.Load(); r != nil {
		return *r
	}
	return ""
}

// noteEngineStartExhausted records that the engine bootstrap spent every
// start attempt it had, and clearEngineStartExhausted takes it back.
//
// The pair the refusal above is a sibling of: that one is "no engine was
// ever built", this one is "one was built and every attempt failed"
// (waired-agent#1093). Neither is the give-up latch — after either of
// them EnsureRunning will still try again on the next trigger, which is
// what adopts an engine installed after boot, and the whole reason the
// latch must not be set from here.
//
// reason is read back off the adapter rather than recomposed, so the
// setup projection quotes the same bytes runtimes[].last_error carries.
//
// Cleared at the top of every start attempt, not on success: while the
// retry loop is running the honest answer is "still trying", and a stale
// value outliving its attempt is the failure mode the refusal record
// already had to be careful about.
func (p *agentInferenceProvider) noteEngineStartExhausted(reason string) {
	if p == nil || reason == "" {
		return
	}
	p.engineStartExhausted.Store(&reason)
}

func (p *agentInferenceProvider) clearEngineStartExhausted() {
	if p == nil {
		return
	}
	p.engineStartExhausted.Store(nil)
}

// engineStartExhaustedReason is the recorded reason, or "".
func (p *agentInferenceProvider) engineStartExhaustedReason() string {
	if p == nil {
		return ""
	}
	if r := p.engineStartExhausted.Load(); r != nil {
		return *r
	}
	return ""
}

// addRefusedEngineRow puts a runtimes[] entry on the map for an engine
// whose bootstrap refused before it built an adapter (waired-agent#1075).
//
// runtimeStatusFor is driven by registry.Names(), and the vLLM adapter is
// registered in the same breath as it is constructed — so a bootstrap that
// refuses earlier leaves the registry holding ollama alone. Every surface
// that reports an engine problem reads runtimes[]: `waired status`'s ⚠
// line, `waired runtimes ls`/`status`, the wizard's terminal arm through
// engineFailureDetail, and the tray through servingRuntime. With no row
// they had nothing to say, and endpointState kept a recorded endpoint at
// "ready" because an absent runtime entry leaves the record alone.
//
// One synthesised row answers all of them, so this needs no new field on
// the wire. It is added only when the registry has no entry for the
// serving engine AND a refusal is recorded, which cannot both hold once an
// adapter exists.
func (p *agentInferenceProvider) addRefusedEngineRow(
	rs map[string]management.RuntimeStatus, hwProfile hardware.Profile,
) {
	if p == nil || rs == nil || p.servingAdapter() != nil {
		return
	}
	reason := p.engineBootstrapRefused()
	if reason == "" {
		return
	}
	name := p.servingEngine()
	if _, ok := rs[name]; ok {
		return
	}
	rs[name] = management.RuntimeStatus{
		Name:      name,
		Installed: engineUsableOnHost(name, hwProfile, p.ollamaUsable, p.vllmUsable),
		State:     infruntime.StateFailed,
		LastError: reason,
	}
}
