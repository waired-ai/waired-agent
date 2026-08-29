package main

import (
	"context"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// latchRecorder captures what LatchFailed was handed, which is the whole
// question in waired-agent#1069: the give-up message is what the surfaces
// quote, and the named cause was not in it.
type latchRecorder struct {
	mu     sync.Mutex
	health string
	latch  string
}

func (a *latchRecorder) Name() string                        { return "vllm" }
func (a *latchRecorder) BaseURL() string                     { return "http://127.0.0.1:9479" }
func (a *latchRecorder) EnsureRunning(context.Context) error { return nil }

// Stop models what the real adapters do, which is the whole point of the
// fake for waired-agent#1138: both assign the WHOLE Health struct with no
// give-up guard and leave the latch standing (the a.proc == nil branch of Stop
// in internal/runtime/ollama.go and vllm.go). A Stop that only returned nil
// could not fail on the defect it is used to pin.
func (a *latchRecorder) Stop(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.health = infruntime.StateStopped
	return nil
}
func (a *latchRecorder) Health(context.Context) infruntime.Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return infruntime.Health{State: a.health}
}
func (a *latchRecorder) LatchFailed(detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.latch = detail
	a.health = infruntime.StateFailed
}
func (a *latchRecorder) FailureLatchedReason() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latch != "", a.latch
}

// FailureLatched is the bool-only half both real adapters also expose.
// Present here so a predicate that asserts on the narrower interface sees the
// same shape in a test as in production; the compile-time assertions in
// engine_dead_test.go and inference_vllm_linux_test.go pin that the two agree.
func (a *latchRecorder) FailureLatched() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latch != ""
}
func (a *latchRecorder) latched() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latch
}

// TestEngineGiveUpMessage_CarriesTheDiagnosisFirst pins the composition.
// PRODUCT CONTRACT (waired-agent#1069): the give-up sentence carries the
// diagnosis, it does not replace it.
//
// The empty-diagnosis row is the compatibility half: an engine failing for
// a reason nothing recognises must be worded exactly as the four
// fmt.Sprintf calls this replaced worded it.
func TestEngineGiveUpMessage_CarriesTheDiagnosisFirst(t *testing.T) {
	const detail = "vllm: process exited during startup: exit status 1\n--- vllm stderr ---\nboom"
	const why = "another program is already listening on 127.0.0.1:9479"

	withWhy := engineStartGiveUp(why, 4, 5*time.Minute, detail)
	if !strings.HasPrefix(withWhy, why+"\n") {
		t.Fatalf("diagnosis is not first:\n%s", withWhy)
	}
	for _, want := range []string{
		"engine failed to start 4 times within 5m0s; automatic restart disabled",
		"waired inference engine start",
		detail,
	} {
		if !strings.Contains(withWhy, want) {
			t.Errorf("missing %q in:\n%s", want, withWhy)
		}
	}

	without := engineStartGiveUp("", 4, 5*time.Minute, detail)
	want := "engine failed to start 4 times within 5m0s; automatic restart disabled" +
		" — see the engine log, then `waired inference engine start` (or switch model) to retry\n" + detail
	if without != want {
		t.Errorf("no-diagnosis wording changed:\ngot  %q\nwant %q", without, want)
	}

	crash := engineCrashGiveUp(why, 4, 5*time.Minute, detail)
	if !strings.HasPrefix(crash, why+"\n") || !strings.Contains(crash, "engine crashed 4 times within 5m0s") {
		t.Errorf("crash shape:\n%s", crash)
	}
}

// TestOnVLLMEngineStartFailed_LatchKeepsTheNamedCause is the defect in
// waired-agent#1069, at the level it was observed: the busy-port sentence
// is on the surfaces right up until the engine gives up, and then it is
// replaced by the give-up message — which is exactly when someone goes
// looking.
//
// There was no test for onVLLMEngineStartFailed at all before this.
func TestOnVLLMEngineStartFailed_LatchKeepsTheNamedCause(t *testing.T) {
	a := &latchRecorder{health: infruntime.StateStarting}
	p := vllmServingProvider(t, a)
	p.cfg = agentconfig.Config{Inference: agentconfig.InferenceConfig{VLLMPort: 9479}}.Inference

	// The detail the adapter reports already carries the engine-log tail,
	// which is where the recognisable line lives.
	detail := "vllm: process exited during startup: exit status 1\n" +
		"--- vllm stderr (tail, full log: /var/lib/waired/runtimes/vllm/logs/engine.log) ---\n" +
		"OSError: [Errno 98] Address already in use"

	for i := 0; i < engineRecoveryMaxAttempts; i++ {
		p.onVLLMEngineStartFailed(detail)
		if got := a.latched(); got != "" {
			t.Fatalf("latched on attempt %d, before the budget ran out: %q", i+1, got)
		}
	}
	p.onVLLMEngineStartFailed(detail)

	got := a.latched()
	if got == "" {
		t.Fatal("the budget ran out and nothing latched")
	}
	if !strings.Contains(got, "another program is already listening on 127.0.0.1:9479") {
		t.Errorf("the give-up message lost the named cause:\n%s", got)
	}
	if !strings.Contains(got, "set inference.vllm_port in agent.json") {
		t.Errorf("the give-up message lost the setting to change:\n%s", got)
	}
	if !strings.Contains(got, "automatic restart disabled") {
		t.Errorf("the give-up message lost its own sentence:\n%s", got)
	}
}

// TestOllamaStartupDiagnosis is the table. Silence on anything it does not
// recognise is the contract, not an omission: a wrong hint on a start-up
// failure sends someone to fix something that is not broken, and reads
// exactly as authoritative as a right one (the rule vllmStartupDiagnosis
// states for itself). See that function's doc for the four candidates that
// were considered and left out, and why.
func TestOllamaStartupDiagnosis(t *testing.T) {
	tests := []struct {
		name, log, want string
	}{
		{
			// Captured on real hardware for waired-agent#1069 (Linux,
			// python listener holding the waired-owned port), verbatim.
			name: "a non-ollama holds the port",
			log: "time=2026-08-28T17:00:00Z level=INFO msg=\"server config\"\n" +
				"Error: listen tcp 127.0.0.1:9475: bind: address already in use\n",
			want: "another program is already listening on 127.0.0.1:9475",
		},
		{
			// The OS wording Go's net package inherits. Frozen here as a
			// record; the live check is
			// TestOllamaStartupDiagnosis_MatchesThisOSBindError below,
			// which takes it from the OS instead of trusting this copy
			// (waired-agent#1085).
			name: "the Windows phrasing of the same thing",
			log:  "Error: listen tcp 127.0.0.1:9475: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.\n",
			want: "another program is already listening on 127.0.0.1:9475",
		},
		{name: "an unrecognised failure stays silent", log: "Error: something new\n", want: ""},
		{name: "an empty log stays silent", log: "", want: ""},
		{
			// Fixed on two axes (the LaunchDaemon plist and ChildBaseEnv),
			// so it is a regression a test should catch, not a sentence to
			// show a person.
			name: "the fixed $HOME cause is not an arm",
			log:  "Error: $HOME is not defined\n",
			want: "",
		},
		{
			// Cannot reach engine.log at all: the kernel kills that
			// process at exec. And it cannot be told from an OOM kill.
			name: "signal: killed is not an arm",
			log:  "ollama: process exited during startup: signal: killed\n",
			want: "",
		},
		{
			// Belongs to the other engine's table; ollama must not borrow
			// a sentence it has no evidence for.
			name: "a vLLM sentence is not an ollama diagnosis",
			log:  "ValueError: No available memory for the cache blocks\n",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaStartupDiagnosis(tc.log, "127.0.0.1:9475")
			switch {
			case tc.want == "" && got != "":
				t.Errorf("want silence, got %q", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("got %q, want it to contain %q", got, tc.want)
			case tc.want != "" && !strings.Contains(got, "inference.ollama_port"):
				t.Errorf("got %q, want it to name the setting to change", got)
			}
		})
	}
}

// TestOllamaStartupDiagnosis_MatchesThisOSBindError takes the busy-port
// wording from the OS rather than trusting the table above.
//
// The table's rows are captures, and a capture ages: the substrings they
// pin were read off one host on one day, and nothing re-reads them. This
// test does. `ollama serve` binds through Go's net package, so the error a
// second Listen on the same address produces here is character-for-
// character the one ollama writes to engine.log — which makes the OS
// itself the fixture, on whichever OS the suite is running.
//
// That matters most on Windows, where the arm was written from the
// documented WSAEADDRINUSE wording rather than from a Waired host
// (waired-agent#1085): the windows CI leg runs `go test ./...` natively,
// so this measures it on every pull request and goes red if a future
// Windows or Go release rewords it.
//
// The t.Logf is for a human running this with -v; CI does not pass -v, so
// on a pass it prints nothing. The failure message is where the captured
// text surfaces, which is the case that needs it — the recorded verbatim
// strings live in ollamaStartupDiagnosis' doc and in
// docs/knowledges/20260828/1900-engine-failure-detail-carries-the-log-tail.md.
func TestOllamaStartupDiagnosis_MatchesThisOSBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	second, err := net.Listen("tcp", addr)
	if err == nil {
		_ = second.Close()
		// Not a skip: this test's whole subject is that binding a held
		// address fails, and an OS where it does not is one where the
		// diagnosis can never fire.
		t.Fatalf("a second listen on %s succeeded; this OS does not refuse a held address", addr)
	}
	t.Logf("%s bind-in-use: %q", runtime.GOOS, err.Error())

	// The prefix ollama's own CLI adds before the net error, so the input
	// is the shape engine.log actually holds.
	line := "Error: " + err.Error() + "\n"
	want := enginePortBusyDiagnosis(addr, "inference.ollama_port")
	if got := ollamaStartupDiagnosis(line, addr); got != want {
		t.Fatalf("ollamaStartupDiagnosis(%q) = %q, want %q\n"+
			"the arm no longer matches what %s says when a port is held",
			line, got, want, runtime.GOOS)
	}
}

// TestEnginePortBusyDiagnosis_IsOneSentenceForBothEngines pins that the two
// engines share the builder. The vLLM wording is quoted verbatim in
// docs-site's troubleshooting page, so it must not move; a second copy is
// how a fix to one silently stops matching the docs.
func TestEnginePortBusyDiagnosis_IsOneSentenceForBothEngines(t *testing.T) {
	const want = "another program is already listening on 127.0.0.1:9479, " +
		"the port the inference engine was told to use — " +
		"set inference.vllm_port in agent.json to a free port"
	if got := enginePortBusyDiagnosis("127.0.0.1:9479", "inference.vllm_port"); got != want {
		t.Errorf("the documented vLLM sentence moved:\ngot  %q\nwant %q", got, want)
	}
	if got := ollamaStartupDiagnosis(
		"Error: listen tcp 127.0.0.1:9475: bind: address already in use", "127.0.0.1:9475",
	); !strings.Contains(got, "inference.ollama_port") {
		t.Errorf("ollama arm names the wrong setting: %q", got)
	}
}

// TestDiagnoseEngineFailure_RoutesByEngine pins that each engine reads its
// own table, and that a detail carrying an engine-log tail is enough — no
// file is re-read, which is what let the diagnosis move into the give-up
// message without any new adapter API.
func TestDiagnoseEngineFailure_RoutesByEngine(t *testing.T) {
	const vllmBusy = "vllm: process exited during startup: exit status 1\n" +
		"--- vllm stderr (tail) ---\nOSError: [Errno 98] Address already in use"
	const ollamaBusy = "ollama: process exited during startup: exit status 1\n" +
		"--- ollama serve stderr (tail) ---\n" +
		"Error: listen tcp 127.0.0.1:9475: bind: address already in use"

	got := diagnoseEngineFailure(catalog.RuntimeVLLM, vllmBusy, "127.0.0.1:9479")
	if !strings.Contains(got, "127.0.0.1:9479") || !strings.Contains(got, "inference.vllm_port") {
		t.Errorf("vllm: got %q", got)
	}
	got = diagnoseEngineFailure(catalog.RuntimeOllama, ollamaBusy, "127.0.0.1:9475")
	if !strings.Contains(got, "127.0.0.1:9475") || !strings.Contains(got, "inference.ollama_port") {
		t.Errorf("ollama: got %q", got)
	}

	// Each engine's table is its own: the vLLM KV-cache arm has no ollama
	// counterpart, and borrowing it would be a hint with no evidence.
	const vllmKV = "ValueError: No available memory for the cache blocks"
	if got := diagnoseEngineFailure(catalog.RuntimeOllama, vllmKV, "127.0.0.1:9475"); got != "" {
		t.Errorf("ollama borrowed a vLLM sentence: %q", got)
	}
	if got := diagnoseEngineFailure("nonesuch", vllmBusy, "127.0.0.1:1"); got != "" {
		t.Errorf("unknown engine: want silence, got %q", got)
	}
}
