package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// What vLLM's start-up needs from the HOST, and how waired reports it
// (waired-agent#898).
//
// The install used to complete, verify, and report success on a host
// where the engine could never start. VLLMVerifyImports checks that vllm
// and torch import, that the GPU is usable, and that the venv can
// download weights — all four pass on a host with no compiler at all,
// because the torch wheels carry their own CUDA RUNTIME and that is all
// torch.cuda.is_available() needs.
//
// What actually starts the engine needs a COMPILER: flashinfer
// JIT-compiles CUDA ops during _initialize_kv_caches, shells out to
// `ninja` (which the pin set installs into the venv), and ninja drives
// `nvcc`, and nvcc drives the host's `g++` for the C++ half. Two of
// those three come from the host and neither was checked.
//
// These are ADVISORIES, not refusals. The ~6 GB venv is a valid artifact
// without them, an operator may be about to install a toolkit, and
// refusing would throw away a build they can still use. What was wrong
// before was the silence, not the success.

// vllmAdvisoryPrefix marks a line carrying an advisory, so the CLI can
// lift them out of the install's ordinary progress chatter.
const vllmAdvisoryPrefix = "toolchain: "

// hostToolchain is what a scan of the host found. Empty strings mean
// "not found", never "not looked for".
type hostToolchain struct {
	// CXX is the path to g++, the compiler nvcc drives for the C++ half.
	// gcc alone does not satisfy this: nvcc needs cc1plus.
	CXX string
	// NVCC is the path to a host CUDA compiler, and NVCCFrom records
	// which of the three places it was found in — that is the part an
	// operator needs when the answer surprises them.
	NVCC     string
	NVCCFrom string
}

// bundledCUDA is the CUDA the venv itself carries, read from the
// nvidia/cu* wheel bundle. Both fields are "major.minor", empty when
// unreadable.
//
// It exists only to be REPORTED. The bundle is not a usable CUDA_HOME:
// it has no lib64 and no libcudart.so, and — as the advisory below says
// — its compiler and its headers can disagree, because they arrive as
// separate wheels resolved against different constraints.
type bundledCUDA struct {
	NVCCVersion   string
	HeaderVersion string
}

var (
	nvccReleaseRe   = regexp.MustCompile(`release (\d+)\.(\d+)`)
	cudartVersionRe = regexp.MustCompile(`#define\s+CUDART_VERSION\s+(\d+)`)
)

// parseNVCCVersion pulls "major.minor" out of `nvcc --version` output.
func parseNVCCVersion(out string) string {
	m := nvccReleaseRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2]
}

// parseCUDARTVersion pulls "major.minor" out of a cuda_runtime_api.h
// header, whose CUDART_VERSION is encoded as major*1000 + minor*10.
func parseCUDARTVersion(header string) string {
	m := cudartVersionRe.FindStringSubmatch(header)
	if m == nil {
		return ""
	}
	var n int
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil || n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d", n/1000, (n%1000)/10)
}

// vllmToolchainAdvisories turns a host scan and a bundled-CUDA reading
// into the lines an operator should see. Empty means nothing to say.
//
// Ordered by what blocks first: without a compiler the engine cannot
// start at all, so those come before the bundle's own inconsistency,
// which only matters to someone who points CUDA_HOME at it.
func vllmToolchainAdvisories(t hostToolchain, b bundledCUDA) []string {
	var out []string
	if t.CXX == "" {
		out = append(out, "no C++ compiler (g++) on this host. nvcc drives g++ for the C++ half of "+
			"vLLM's start-up compile, and gcc alone does not satisfy it (it needs cc1plus). "+
			"Without it the engine will not start. Install g++ (Debian/Ubuntu: apt-get install g++).")
	}
	if t.NVCC == "" {
		out = append(out, "no CUDA toolkit on this host: no nvcc on PATH, under $CUDA_HOME, or at "+
			"/usr/local/cuda. torch ships its own CUDA runtime, which is why the checks above pass, "+
			"but vLLM compiles kernels at engine start and that needs a compiler. Without it the "+
			"engine will not start. Install a CUDA toolkit (Debian/Ubuntu: apt-get install cuda-toolkit-13-1, "+
			"or the cuda-nvcc-* and cuda-cudart-dev-* pair).")
	}
	// Reported, never acted on. Pinning the two wheels to agree is not a
	// one-line change — their version ranges come from different
	// dependencies of vllm itself, and forcing one moves a constraint
	// somebody else declared. It stays inert as long as a host toolkit
	// is present, because that is what nvcc and the headers are then
	// taken from.
	if b.NVCCVersion != "" && b.HeaderVersion != "" && b.NVCCVersion != b.HeaderVersion {
		out = append(out, fmt.Sprintf(
			"the CUDA bundled inside the venv is inconsistent: its nvcc is %s but its runtime headers "+
				"are %s, and a compile using both is rejected. This is harmless while a host CUDA toolkit "+
				"is present, because that is where the compiler and headers come from — but do NOT set "+
				"CUDA_HOME to the venv's nvidia/cu* directory to work around a missing toolkit: it has no "+
				"lib64 and no libcudart.so either.", b.NVCCVersion, b.HeaderVersion))
	}
	return out
}

// readBundledCUDA reads the venv's own CUDA bundle. Best effort: an
// unreadable or absent bundle yields zero values and no advisory,
// because "I could not look" must not be reported as "they disagree".
func readBundledCUDA(venvDir string, runVersion func(nvcc string) string) bundledCUDA {
	matches, err := filepath.Glob(filepath.Join(venvDir, "lib", "python*", "site-packages", "nvidia", "cu*"))
	if err != nil {
		return bundledCUDA{}
	}
	for _, dir := range matches {
		nvcc := filepath.Join(dir, "bin", "nvcc")
		header := filepath.Join(dir, "include", "cuda_runtime_api.h")
		if _, err := os.Stat(nvcc); err != nil {
			continue
		}
		raw, err := os.ReadFile(header)
		if err != nil {
			continue
		}
		return bundledCUDA{
			NVCCVersion:   parseNVCCVersion(runVersion(nvcc)),
			HeaderVersion: parseCUDARTVersion(string(raw)),
		}
	}
	return bundledCUDA{}
}

// formatAdvisories prefixes each line so a caller streaming install
// output can pick them out of the ordinary chatter.
func formatAdvisories(msgs []string) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if strings.TrimSpace(m) == "" {
			continue
		}
		out = append(out, vllmAdvisoryPrefix+m)
	}
	return out
}
