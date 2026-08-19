//go:build linux

package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func vllmHTTPClient() *http.Client {
	return &http.Client{Timeout: 200 * time.Millisecond}
}

// vllmFakeServer returns a httptest server whose /health and /v1/models
// behaviour the test can swap atomically.
type vllmFakeServer struct {
	srv         *httptest.Server
	healthy     atomic.Bool
	servedName  atomic.Value // string
	modelsCalls atomic.Int32
}

func newVLLMFakeServer(servedName string) *vllmFakeServer {
	f := &vllmFakeServer{}
	f.servedName.Store(servedName)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if f.healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		f.modelsCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"` + f.servedName.Load().(string) + `"}]}`))
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *vllmFakeServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	return splitHostPort(t, f.srv.URL)
}

func TestVLLMAdapter_EnsureRunning_Success(t *testing.T) {
	server := newVLLMFakeServer("qwen3-32b-instruct")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python:               "/venv/bin/python",
		Host:                 host,
		Port:                 port,
		Model:                "/models/qwen3-32b/awq",
		ServedModelName:      "qwen3-32b-instruct",
		MaxModelLen:          8192,
		GPUMemoryUtilization: 0.85,
		Spawner:              spawner,
		HTTPClient:           vllmHTTPClient(),
		HealthInterval:       10 * time.Millisecond,
		HealthSuccess:        2,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if a.Health(ctx).State != StateReady {
		t.Errorf("state = %+v, want StateReady", a.Health(ctx))
	}
	if spawner.calls != 1 {
		t.Errorf("spawner calls = %d, want 1", spawner.calls)
	}
	if spawner.lastBin != "/venv/bin/python" {
		t.Errorf("python path = %q", spawner.lastBin)
	}
	if !sliceContains(spawner.lastArgs, "vllm.entrypoints.openai.api_server") {
		t.Errorf("missing -m vllm.entrypoints.openai.api_server arg, got %v", spawner.lastArgs)
	}
	if !sliceContains(spawner.lastArgs, "--no-enable-log-requests") {
		t.Errorf("missing --no-enable-log-requests, got %v", spawner.lastArgs)
	}
	if !sliceContains(spawner.lastArgs, "0.85") {
		t.Errorf("missing gpu-memory-utilization 0.85, got %v", spawner.lastArgs)
	}
	if !sliceContains(spawner.lastArgs, "/models/qwen3-32b/awq") {
		t.Errorf("missing --model path, got %v", spawner.lastArgs)
	}
	if !sliceContains(spawner.lastArgs, "qwen3-32b-instruct") {
		t.Errorf("missing --served-model-name, got %v", spawner.lastArgs)
	}
}

// commandArgs is exercised directly (no fake server) so the argv
// contract — flag omission at TP 0/1, the explicit prefix-caching pin,
// and the absence of the removed --quantization flag — is pinned.
func TestVLLMCommandArgs(t *testing.T) {
	base := VLLMConfig{
		Python:          "/venv/bin/python",
		Host:            "127.0.0.1",
		Port:            8000,
		Model:           "/models/m",
		ServedModelName: "m",
		MaxModelLen:     131072,
		DType:           "auto",
	}

	t.Run("single GPU omits --tensor-parallel-size", func(t *testing.T) {
		for _, tp := range []int{0, 1} {
			a := NewVLLMAdapter(base)
			a.cfg.TensorParallelSize = tp
			args := a.commandArgs()
			if sliceContains(args, "--tensor-parallel-size") {
				t.Errorf("TP=%d: --tensor-parallel-size present, want omitted: %v", tp, args)
			}
		}
	})

	t.Run("TP=2 appends --tensor-parallel-size 2", func(t *testing.T) {
		cfg := base
		cfg.TensorParallelSize = 2
		args := NewVLLMAdapter(cfg).commandArgs()
		found := false
		for i, arg := range args {
			if arg == "--tensor-parallel-size" {
				found = true
				if i+1 >= len(args) || args[i+1] != "2" {
					t.Errorf("--tensor-parallel-size value wrong: %v", args)
				}
			}
		}
		if !found {
			t.Errorf("missing --tensor-parallel-size: %v", args)
		}
	})

	t.Run("prefix caching pinned on", func(t *testing.T) {
		args := NewVLLMAdapter(base).commandArgs()
		if !sliceContains(args, "--enable-prefix-caching") {
			t.Errorf("missing --enable-prefix-caching: %v", args)
		}
	})

	t.Run("no --quantization (auto-detected from HF config)", func(t *testing.T) {
		args := NewVLLMAdapter(base).commandArgs()
		if sliceContains(args, "--quantization") {
			t.Errorf("--quantization should no longer be emitted: %v", args)
		}
	})

	t.Run("KVCacheDType empty omits --kv-cache-dtype", func(t *testing.T) {
		args := NewVLLMAdapter(base).commandArgs()
		if sliceContains(args, "--kv-cache-dtype") {
			t.Errorf("empty KVCacheDType must omit the flag: %v", args)
		}
	})

	t.Run("KVCacheDType fp8 appends --kv-cache-dtype fp8", func(t *testing.T) {
		cfg := base
		cfg.KVCacheDType = "fp8"
		args := NewVLLMAdapter(cfg).commandArgs()
		if !argPairPresent(args, "--kv-cache-dtype", "fp8") {
			t.Errorf("missing --kv-cache-dtype fp8: %v", args)
		}
	})

	t.Run("SpeculativeConfig empty omits --speculative-config", func(t *testing.T) {
		args := NewVLLMAdapter(base).commandArgs()
		if sliceContains(args, "--speculative-config") {
			t.Errorf("empty SpeculativeConfig must omit the flag: %v", args)
		}
	})

	t.Run("SpeculativeConfig appends --speculative-config verbatim", func(t *testing.T) {
		cfg := base
		cfg.SpeculativeConfig = `{"method":"ngram","num_speculative_tokens":5}`
		args := NewVLLMAdapter(cfg).commandArgs()
		if !argPairPresent(args, "--speculative-config", cfg.SpeculativeConfig) {
			t.Errorf("missing --speculative-config %s: %v", cfg.SpeculativeConfig, args)
		}
	})

	// Product contract (#410), not a record of today's behaviour: vLLM
	// rejects --enable-auto-tool-choice without --tool-call-parser, and
	// ignores --tool-call-parser without it, so the pair is emitted
	// together or not at all.
	t.Run("ToolCallParser empty omits both tool-calling flags", func(t *testing.T) {
		args := NewVLLMAdapter(base).commandArgs()
		if sliceContains(args, "--tool-call-parser") {
			t.Errorf("empty ToolCallParser must omit --tool-call-parser: %v", args)
		}
		if sliceContains(args, "--enable-auto-tool-choice") {
			t.Errorf("empty ToolCallParser must omit --enable-auto-tool-choice: %v", args)
		}
	})

	t.Run("ToolCallParser appends --enable-auto-tool-choice and the parser", func(t *testing.T) {
		cfg := base
		cfg.ToolCallParser = "qwen3_xml"
		args := NewVLLMAdapter(cfg).commandArgs()
		if !sliceContains(args, "--enable-auto-tool-choice") {
			t.Errorf("missing --enable-auto-tool-choice: %v", args)
		}
		if !argPairPresent(args, "--tool-call-parser", "qwen3_xml") {
			t.Errorf("missing --tool-call-parser qwen3_xml: %v", args)
		}
	})

	// Product contract (waired-agent#885): the flag is what makes vLLM
	// populate usage.prompt_tokens_details.cached_tokens at all — without
	// it the whole block comes back null. The negative case matters as
	// much as the positive one: the caller gates it on the installed
	// venv's version, and an unrecognised flag is an argparse exit 2 that
	// takes the engine down rather than one feature.
	t.Run("EnablePromptTokensDetails false omits the flag", func(t *testing.T) {
		args := NewVLLMAdapter(base).commandArgs()
		if sliceContains(args, "--enable-prompt-tokens-details") {
			t.Errorf("flag emitted while disabled: %v", args)
		}
	})

	t.Run("EnablePromptTokensDetails true emits the bare flag", func(t *testing.T) {
		cfg := base
		cfg.EnablePromptTokensDetails = true
		args := NewVLLMAdapter(cfg).commandArgs()
		if !sliceContains(args, "--enable-prompt-tokens-details") {
			t.Errorf("missing --enable-prompt-tokens-details: %v", args)
		}
	})
}

// argPairPresent reports whether args contains flag immediately followed
// by value.
func argPairPresent(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TP > 1 widens the default readiness budget (NCCL init + per-worker
// JIT on top of the weight load); explicit values still win.
func TestVLLMHealthMaxFailsDefault(t *testing.T) {
	if got := NewVLLMAdapter(VLLMConfig{}).cfg.HealthMaxFails; got != 60 {
		t.Errorf("single-GPU default HealthMaxFails = %d, want 60", got)
	}
	if got := NewVLLMAdapter(VLLMConfig{TensorParallelSize: 2}).cfg.HealthMaxFails; got != 90 {
		t.Errorf("TP=2 default HealthMaxFails = %d, want 90", got)
	}
	if got := NewVLLMAdapter(VLLMConfig{TensorParallelSize: 2, HealthMaxFails: 5}).cfg.HealthMaxFails; got != 5 {
		t.Errorf("explicit HealthMaxFails = %d, want 5 (explicit wins)", got)
	}
}

func TestVLLMAdapter_EnsureRunning_ServedModelMismatch(t *testing.T) {
	server := newVLLMFakeServer("wrong-name")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "qwen3-32b-instruct",
		Spawner:        &fakeSpawner{},
		HTTPClient:     vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  2,
		StopTimeout:    50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := a.EnsureRunning(ctx)
	if err == nil || !strings.Contains(err.Error(), "served model name") {
		t.Errorf("expected served-model-name mismatch error, got %v", err)
	}
}

func TestVLLMAdapter_EnsureRunning_HealthTimeout(t *testing.T) {
	server := newVLLMFakeServer("qwen3-32b-instruct")
	defer server.srv.Close()
	// stays unhealthy
	host, port := server.hostPort(t)

	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "qwen3-32b-instruct",
		Spawner:        &fakeSpawner{},
		HTTPClient:     vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  2,
		HealthMaxFails: 3,
		StopTimeout:    50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err == nil {
		t.Errorf("expected timeout error when /health never returns 200")
	}
	if a.Health(ctx).State != StateFailed {
		t.Errorf("state = %+v, want StateFailed", a.Health(ctx))
	}
}

func TestVLLMAdapter_Stop_SendsSIGTERM(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "x",
		Spawner:        spawner,
		HTTPClient:     vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		StopTimeout:    100 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		spawner.process.exit(nil)
	}()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	signals := spawner.process.sentSignals()
	if len(signals) == 0 || signals[0] != syscall.SIGTERM {
		t.Errorf("expected SIGTERM first, got %v", signals)
	}
	if a.Health(context.Background()).State != StateStopped {
		t.Errorf("state = %+v, want StateStopped", a.Health(context.Background()))
	}
}

func TestVLLMAdapter_Stop_SIGKILLEscalation(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "x",
		Spawner: spawner, HTTPClient: vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
		StopTimeout: 30 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	// Don't exit the fake process — Stop must escalate to Kill.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !spawner.process.wasKilled() {
		t.Errorf("expected SIGKILL escalation when process didn't exit before StopTimeout")
	}
}

// TestVLLMAdapter_Stop_KillsWhenCallerCancels pins the same commit-to-kill
// contract as the Ollama adapter (#316): vLLM's multiprocessing workers
// hold GPU memory, so a caller that gives up mid-stop must not leave them
// running. PRODUCT CONTRACT, not a record of today's behaviour.
func TestVLLMAdapter_Stop_KillsWhenCallerCancels(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "x",
		Spawner: spawner, HTTPClient: vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
		StopTimeout: 30 * time.Second, // never reached: the caller gives up first
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop after caller cancellation = %v, want nil (the memory WAS freed)", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Stop waited %s; it must escalate as soon as the caller gives up", elapsed)
	}
	if !spawner.process.wasKilled() {
		t.Errorf("caller cancellation abandoned the vLLM workers alive")
	}
}

func TestVLLMAdapter_Idempotent(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "x",
		Spawner: spawner, HTTPClient: vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("first EnsureRunning: %v", err)
	}
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("second EnsureRunning (idempotent): %v", err)
	}
	if spawner.calls != 1 {
		t.Errorf("spawner.calls = %d, want 1 (second call should be no-op)", spawner.calls)
	}
}

// TestVLLMAdapter_EnsureRunning_ConcurrentCallersJoin is the PRODUCT
// CONTRACT that replaces the old "EnsureRunning called while already
// starting" hard error (#280): a caller that arrives during a start joins
// it instead of erroring. The old error became a 503 runtime_unhealthy for
// every request that arrived while the engine was starting — and a restart
// is precisely what makes arrival concurrent. Ratified shape:
// TestOllamaAdapter_EnsureRunning_ConcurrentCallersJoin (#279).
func TestVLLMAdapter_EnsureRunning_ConcurrentCallersJoin(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "x",
		Spawner: spawner, HTTPClient: vllmHTTPClient(),
		// A slow readiness wait widens the window callers race into.
		HealthInterval: 20 * time.Millisecond, HealthSuccess: 3, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	defer func() { _ = a.Stop(context.Background()) }()

	const n = 12
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.EnsureRunning(context.Background()); err != nil {
				failures.Add(1)
				t.Errorf("concurrent EnsureRunning returned %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Errorf("%d of %d concurrent callers failed", f, n)
	}
	spawner.mu.Lock()
	calls := spawner.calls
	spawner.mu.Unlock()
	if calls != 1 {
		t.Errorf("spawner.calls = %d, want 1 — the joiners must not each spawn an engine", calls)
	}
}

// TestVLLMAdapter_EnsureRunning_RetriesAfterFailed pins that the join gate
// is per-attempt: a failed start clears it, so a later call becomes a fresh
// leader and respawns. The daemon's bounded start-retry loop depends on
// this — a gate that latched failure would turn one bad start into a
// permanent error (#280).
func TestVLLMAdapter_EnsureRunning_RetriesAfterFailed(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	// stays unhealthy: the first start must fail
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/m", ServedModelName: "x",
		Spawner: spawner, HTTPClient: vllmHTTPClient(),
		HealthInterval: 10 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 3,
		StopTimeout: 50 * time.Millisecond,
	})
	defer func() { _ = a.Stop(context.Background()) }()

	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatal("first EnsureRunning succeeded against an unhealthy server, want error")
	}
	if st := a.Health(context.Background()).State; st != StateFailed {
		t.Fatalf("state after failed start = %v, want StateFailed", st)
	}

	server.healthy.Store(true)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("second EnsureRunning after recovery: %v", err)
	}
	spawner.mu.Lock()
	calls := spawner.calls
	spawner.mu.Unlock()
	if calls != 2 {
		t.Errorf("spawner.calls = %d, want 2 — a failed start must not latch the gate", calls)
	}
}

// TestVLLMAdapter_EngineLog_Captured verifies the #587 fix: with LogDir
// set the adapter opens <LogDir>/engine.log and hands the spawner a
// non-nil capture writer; without LogDir the writer stays nil (discard)
// — the same contract as the ollama adapter.
func TestVLLMAdapter_EngineLog_Captured(t *testing.T) {
	server := newVLLMFakeServer("x")
	defer server.srv.Close()
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	t.Run("with LogDir", func(t *testing.T) {
		dir := t.TempDir()
		spawner := &fakeSpawner{}
		a := NewVLLMAdapter(VLLMConfig{
			Python: "/venv/bin/python", Host: host, Port: port,
			Model: "/m", ServedModelName: "x",
			Spawner: spawner, HTTPClient: vllmHTTPClient(),
			HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
			StopTimeout: 50 * time.Millisecond,
			LogDir:      dir,
		})
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if spawner.lastLogW == nil {
			t.Error("spawner received nil log writer; want a non-nil capture writer when LogDir is set")
		}
		if _, err := os.Stat(filepath.Join(dir, "engine.log")); err != nil {
			t.Errorf("engine.log not created: %v", err)
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			spawner.process.exit(nil)
		}()
		_ = a.Stop(context.Background())
	})

	t.Run("without LogDir", func(t *testing.T) {
		spawner := &fakeSpawner{}
		a := NewVLLMAdapter(VLLMConfig{
			Python: "/venv/bin/python", Host: host, Port: port,
			Model: "/m", ServedModelName: "x",
			Spawner: spawner, HTTPClient: vllmHTTPClient(),
			HealthInterval: 10 * time.Millisecond, HealthSuccess: 1,
			StopTimeout: 50 * time.Millisecond,
		})
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if spawner.lastLogW != nil {
			t.Error("spawner received a non-nil log writer; want nil (discard) when LogDir is unset")
		}
		go func() {
			time.Sleep(20 * time.Millisecond)
			spawner.process.exit(nil)
		}()
		_ = a.Stop(context.Background())
	})
}

func TestVLLMAdapter_NameAndBaseURL(t *testing.T) {
	a := NewVLLMAdapter(VLLMConfig{Host: "127.0.0.1", Port: 8000})
	if a.Name() != "vllm" {
		t.Errorf("Name = %q", a.Name())
	}
	if a.BaseURL() != "http://127.0.0.1:8000" {
		t.Errorf("BaseURL = %q", a.BaseURL())
	}
}

func TestParseGPUOrphans(t *testing.T) {
	in := strings.Join([]string{
		"12345, 18432, python",
		"23456, 16384, /home/u/.local/share/waired/runtimes/vllm/0.11.0/.venv/bin/python",
		"34567, 4096, claude-code", // unrelated, must be skipped
		"",                         // blank line
		"45678, 8000, vllm-worker", // matches "vllm"
	}, "\n") + "\n"
	got := parseGPUOrphans(in)
	if len(got) != 3 {
		t.Fatalf("got %d orphans, want 3 (got=%+v)", len(got), got)
	}
	if got[0].PID != 12345 || got[0].UsedMemMiB != 18432 {
		t.Errorf("orphans[0] = %+v", got[0])
	}
	if got[2].PID != 45678 {
		t.Errorf("orphans[2] = %+v", got[2])
	}
}

func TestVLLMAdapter_AppliedTuningRoundTrip(t *testing.T) {
	a := NewVLLMAdapter(VLLMConfig{})
	if got := a.AppliedTuning(); got != (ModelTuning{}) {
		t.Fatalf("zero value expected before tuning is set, got %+v", got)
	}
	want := ModelTuning{
		ModelID: "gpt-oss-20b", VariantID: "mxfp4",
		ContextLength: 45056, Verified: true,
		Warning: "context window clamped to 45056 tokens",
	}
	a.SetAppliedTuning(want)
	if got := a.AppliedTuning(); got != want {
		t.Errorf("AppliedTuning = %+v, want %+v", got, want)
	}
}
