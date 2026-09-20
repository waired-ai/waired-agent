package runtime

import "testing"

// The tails below are verbatim from engine logs captured on the reference
// host on 2026-09-20 (the runs behind
// docs/knowledges/20260920/0600-windows-igpu-memory-ceiling-below-fit.md).
// Shortened at the top only; nothing inside is edited.
const (
	// The load that failed: f16 KV at 131,072, projected 85,070 MiB.
	failingLoadTail = `sched_reserve:    Vulkan0 compute buffer size =   249.02 MiB
sched_reserve: Vulkan_Host compute buffer size =   140.02 MiB
sched_reserve: graph nodes  = 4471
sched_reserve: graph splits = 2
sched_reserve: reserve took 155.84 ms, sched copies = 1
cmn          init: llama threadpool init, n_threads = 16
cmn  common_init_: KV cache shifting is not supported for this context, disabling KV cache shifting
cmn  common_init_: warming up the model with an empty run - please wait ... (--no-warmup to disable)
time=2026-09-20T04:21:14.475+09:00 level=INFO source=llama_server.go:1350 msg="waiting for llama-server to become available" status="llm server not responding"
time=2026-09-20T04:21:24.789+09:00 level=INFO source=llama_server.go:1350 msg="waiting for llama-server to become available" status="llm server error"
time=2026-09-20T04:21:26.403+09:00 level=INFO source=sched.go:641 msg="Load failed" model=C:\Users\Public\w1443\models\blobs\sha256-0fba error="llama-server process has terminated: exit status 0xe06d7363: NTSTATUS 0xe06d7363"
`
	// The same model at a size that fits: q4_0 KV at 200,704.
	healthyLoadTail = `srv  llama_server: model loaded
srv  llama_server: listening on http://127.0.0.1:64835
srv  update_slots: all slots are idle
time=2026-09-20T04:26:58.365+09:00 level=INFO source=llama_server.go:1362 msg="llama-server started in 56.02 seconds"
time=2026-09-20T04:26:58.433+09:00 level=INFO source=sched.go:728 msg="loaded runners" count=1
`
	// The body ollama answers with in both #1443 shapes.
	deadRunnerBody = `{"error":"llama-server process has terminated: exit status 0xe06d7363: NTSTATUS 0xe06d7363"}`
)

// TestLoadMemoryFailure is the whole specification of the classifier.
//
// The first case is a product contract (waired-agent#1453, owner direction
// 2026-09-20): the reference host's failure MUST classify as memory, because
// classifying it as a dead engine is what made the product restart into the
// same doomed load. The rest are records of what these logs contain.
//
// Note what the second case proves: none of the five failing runs on that
// host printed an allocation failure at all — verified by grep over the
// captured logs — so a classifier built only on allocation markers would
// catch none of them.
func TestLoadMemoryFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts loadMemoryFacts
		want  bool
	}{
		{
			name:  "CONTRACT: the reference host died making the model resident",
			facts: loadMemoryFacts{Body: deadRunnerBody, LogTail: failingLoadTail},
			want:  true,
		},
		{
			name: "an engine that did report a failed allocation",
			facts: loadMemoryFacts{
				Body:    deadRunnerBody,
				LogTail: "ggml_vulkan: Memory allocation of size 2147483648 failed.\n",
			},
			want: true,
		},
		{
			name: "a CUDA host out of memory",
			facts: loadMemoryFacts{
				Body:    deadRunnerBody,
				LogTail: "CUDA error: out of memory\n  current device: 0\n",
			},
			want: true,
		},
		{
			// Deliberately a body WITHOUT "process has terminated": that
			// phrase is ollama's, and it would carry this case through
			// engineDeadMarkers whatever the signal clause did. This is the
			// sentence startupExitError builds, which is what a load killed
			// by the kernel looks like when the supervisor is ours.
			name: "the linux OOM killer took the runner mid-load",
			facts: loadMemoryFacts{
				Body:    "ollama: process exited during startup: signal: killed",
				LogTail: failingLoadTail,
			},
			want: true,
		},
		{
			name: "a runner that died while loading tensors",
			facts: loadMemoryFacts{
				Body:    deadRunnerBody,
				LogTail: "llama_model_loader: loaded meta data\nload_tensors: offloading 48 layers to GPU\n",
			},
			want: true,
		},
		{
			name:  "a runner that died after it was serving",
			facts: loadMemoryFacts{Body: deadRunnerBody, LogTail: healthyLoadTail},
			want:  false,
		},
		{
			name: "a template failure, which must never read as memory",
			facts: loadMemoryFacts{
				Body:    `{"error":"template: :1: function \"tools\" not defined"}`,
				LogTail: healthyLoadTail,
			},
			want: false,
		},
		{
			name: "a tool-parser failure, likewise",
			facts: loadMemoryFacts{
				Body:    `{"error":"error parsing tool call: unexpected end of JSON input"}`,
				LogTail: healthyLoadTail,
			},
			want: false,
		},
		{
			name: "a capped log, whose tail is not the load that just failed",
			facts: loadMemoryFacts{
				Body:      deadRunnerBody,
				LogTail:   failingLoadTail,
				TailStale: true,
			},
			want: false,
		},
		{
			name: "the runner is still alive, so where the log stops says nothing",
			facts: loadMemoryFacts{
				Body:    `{"error":"model requires more system memory than is available"}`,
				LogTail: failingLoadTail,
			},
			want: false,
		},
		{
			name:  "nothing to go on",
			facts: loadMemoryFacts{},
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := loadMemoryFailure(tc.facts)
			if got != tc.want {
				t.Fatalf("loadMemoryFailure() = %v, want %v (reason %q)", got, tc.want, reason)
			}
			if got && reason == "" {
				t.Error("classified as a memory failure with no reason to show an operator")
			}
			if !got && reason != "" {
				t.Errorf("not a memory failure but carried a reason: %q", reason)
			}
		})
	}
}

// TestLastRunnerLine pins the one piece of parsing the positional rule
// stands on: ollama always writes after the runner, so "the last line"
// would otherwise always be ollama's.
func TestLastRunnerLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
		want string
	}{
		{
			name: "skips the supervisor lines under the runner's last word",
			tail: failingLoadTail,
			want: "cmn  common_init_: warming up the model with an empty run - please wait ... (--no-warmup to disable)",
		},
		{
			name: "a healthy load",
			tail: healthyLoadTail,
			want: "srv  update_slots: all slots are idle",
		},
		{
			name: "only supervisor lines",
			tail: "first line is cut\ntime=2026-09-20T04:21:26Z level=INFO msg=\"Load failed\"\n",
			want: "",
		},
		{
			name: "the half line a byte-cut tail starts with is dropped",
			tail: "buffer size =   249.0\nreal line\n",
			want: "real line",
		},
		{
			name: "empty",
			tail: "",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastRunnerLine(tc.tail); got != tc.want {
				t.Errorf("lastRunnerLine() = %q, want %q", got, tc.want)
			}
		})
	}
}
