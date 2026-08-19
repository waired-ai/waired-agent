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
// renovate: datasource=pypi depName=vllm
const VLLMPinnedVersion = "0.24.0"

// HFTransferPinnedVersion is the hf_transfer wheel installed alongside
// vLLM so HF downloads enable the Rust fast path.
// renovate: datasource=pypi depName=hf_transfer
const HFTransferPinnedVersion = "0.1.9"

// TransformersConstraint pins the transformers wheel to a version
// compatible with VLLMPinnedVersion. vllm 0.24.0 requires
// transformers>=5.5.3 (its 0.11-era code needed <5.0 instead — the two
// major lines are mutually incompatible), so the constraint here only
// caps the major to keep uv from resolving a future transformers 6.x
// before it has been verified. Bump together with VLLMPinnedVersion
// after verifying compatibility on a real GPU host.
const TransformersConstraint = "transformers>=5.5.3,<6.0"

// VLLMPythonVersion is the interpreter `uv venv --python` materialises
// for the venv — the Step 2 supported interpreter window. A constant
// rather than a literal inside Install because the converge compares it:
// a venv built on a different interpreter is a different build even when
// the vLLM version matches.
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
