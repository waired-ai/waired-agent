package runtime

// The vLLM pin SET, in one place and on every platform.
//
// These four move together. `uv pip install vllm==X` resolves against
// the interpreter the venv was built with and against the transformers
// line that release requires, so a build is only reproducible as the
// whole tuple — which is why ConvergeVLLM compares the tuple rather than
// the vLLM version alone (#843).
//
// Untagged, and no longer restated in vllm_stub_darwin.go /
// vllm_stub_windows.go. The stubs carried their own copies "so CLI help
// text and confirmation prompts can render the version even where
// Install refuses"; three copies of a constant that must agree is a
// worse answer to that than one copy with no build tag, and the converge
// needs the companion pins on every platform for the same reason.

// VLLMPinnedVersion is the vLLM release Step 2 installs into the
// uv-managed venv, and the one ConvergeVLLM brings an installed venv to.
// Refreshed together with VLLMVerifyImports's SM-capability check
// whenever upstream drops a new Blackwell-aware build. Bumping is a
// documented Step 8 / 14 maintenance task.
//
// Moving it is not free for installed hosts: every one of them rebuilds
// its venv on the next update (#843). That is the point — the product's
// vLLM-facing tables are read out of THIS release's own registry, so a
// host on another version is not "old but working". See the tool-parser
// table in cmd/waired-agent/inference_vllm_toolparser.go: vLLM rejects
// an unregistered --tool-call-parser at start-up, and an unknown CLI
// flag is an argparse exit 2, so a single missing name or flag costs the
// whole engine rather than one feature.
//
// 0.24.0 -> 0.28.0, validated 2026-08-29 on sv-mag (RTX PRO 4000
// Blackwell, compute capability 12.0) by replaying THIS product's exact
// serve argv against a scratch venv (waired-agent#1133). What came out
// of that, in the order it costs:
//
//   - Every flag VLLMAdapter.commandArgs can emit is still accepted;
//     the engine's own "non-default args" line echoed all of them and
//     argparse rejected none. --no-enable-log-requests, the newest
//     rename, and the --kv-offloading pair included.
//   - All five --tool-call-parser names this build can pass are still in
//     _TOOL_PARSERS_TO_REGISTER (48 registered), and a live tool call
//     came back as a structured tool_calls array with
//     finish_reason=tool_calls.
//   - vllmKVCapacityRe still matches: "GPU KV cache size: 339,160
//     tokens" (0.24.0 reported 393,709 for the same argv and card — the
//     pool is 14% smaller, which is a real change and the reason #1131
//     ships with this).
//   - The engine's own defaults for a sub-70 GiB card are unchanged at
//     max_num_batched_tokens=2048 and max_num_seqs=256, so our 4096
//     still raises the prefill chunk rather than lowering it. Upstream
//     grew a THIRD tier though: >= 160 GiB now defaults to 16384, above
//     the 8192 router.VLLMMaxNumBatchedTokens picks for a big GPU. No card this
//     product serves on is there yet.
//   - kv_offloading_backend still defaults to native, not lmcache.
//
// And the one that decides the shape of the install below: 0.24.0
// declared BOTH flashinfer-python and flashinfer-cubin; 0.28.0 declares
// only flashinfer-python. vLLM's has_flashinfer() accepts either the
// cubin package OR nvcc on PATH, so dropping the first makes the engine
// depend on the second — and on a host where nvcc is installed but not
// on PATH (the default for /usr/local/cuda on Ubuntu) 0.28.0 dies at
// start-up with "FlashInfer backend is not available". Reproduced on
// sv-mag, which has nvcc at /usr/local/cuda/bin and neither the user's
// nor root's PATH pointing at it. Pinning the cubin package back is not
// available: PyPI's newest flashinfer-cubin is 0.6.13 and 0.28.0 wants
// flashinfer-python 0.6.16.post3. The fix is therefore on our side —
// the child env puts the host's nvcc directory on PATH — and it is not
// optional, because without it every host that takes this pin loses
// local inference entirely.
//
// renovate: datasource=pypi depName=vllm
const VLLMPinnedVersion = "0.28.0"

// HFTransferPinnedVersion is the hf_transfer wheel installed alongside
// vLLM so HF downloads enable the Rust fast path.
// renovate: datasource=pypi depName=hf_transfer
const HFTransferPinnedVersion = "0.1.9"

// TransformersConstraint pins the transformers wheel to a version
// compatible with VLLMPinnedVersion. vllm 0.28.0 requires
// transformers>=5.5.3, the same floor 0.24.0 stated (its 0.11-era code
// needed <5.0 instead — the two major lines are mutually incompatible),
// so the constraint here only caps the major to keep uv from resolving a
// future transformers 6.x before it has been verified. Bump together
// with VLLMPinnedVersion after verifying compatibility on a real GPU
// host.
//
// Unchanged at 0.28.0, and the cap is doing work rather than sitting
// idle: the verified venv resolved transformers 5.16.1, so the range is
// live at its top end, not pinned at its floor.
const TransformersConstraint = "transformers>=5.5.3,<6.0"

// VLLMPythonVersion is the interpreter `uv venv --python` materialises
// for the venv — the Step 2 supported interpreter window. A constant
// rather than a literal inside Install because the converge compares it:
// a venv built on a different interpreter is a different build even when
// the vLLM version matches.
//
// 0.28.0 declares requires_python <3.15,>=3.10, so 3.12 stays inside it.
const VLLMPythonVersion = "3.12"

// VLLMPinSet is the tuple one venv was built from. Recorded beside the
// venv at install time and compared on every converge, so that moving a
// companion pin without moving VLLMPinnedVersion still reaches installed
// hosts (#843) — the vLLM version alone cannot key it, because the venv
// directory is named after that version and would look up to date.
//
// JSON field names are the wheel/tool names rather than the Go field
// names so the file reads like the install request it records.
type VLLMPinSet struct {
	VLLM         string `json:"vllm"`
	HFTransfer   string `json:"hf_transfer"`
	Transformers string `json:"transformers"`
	Python       string `json:"python"`
}

// WantedVLLMPins is the set this build installs.
func WantedVLLMPins() VLLMPinSet {
	return VLLMPinSet{
		VLLM:         VLLMPinnedVersion,
		HFTransfer:   HFTransferPinnedVersion,
		Transformers: TransformersConstraint,
		Python:       VLLMPythonVersion,
	}
}
