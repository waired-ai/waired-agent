//go:build linux

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// detectHostToolchain scans for the two things the start-up compile
// needs from the host (waired-agent#898), in the order flashinfer's own
// lookup uses: $CUDA_HOME / $CUDA_PATH first, then PATH, then the
// conventional /usr/local/cuda.
//
// Deliberately mirrors that order rather than picking the "best" one, so
// what this reports is what the engine will actually use.
func detectHostToolchain() hostToolchain {
	var t hostToolchain
	if p, err := exec.LookPath("g++"); err == nil {
		t.CXX = p
	}
	for _, env := range []string{"CUDA_HOME", "CUDA_PATH"} {
		home := strings.TrimSpace(os.Getenv(env))
		if home == "" {
			continue
		}
		cand := filepath.Join(home, "bin", "nvcc")
		if _, err := os.Stat(cand); err == nil {
			t.NVCC, t.NVCCFrom = cand, "$"+env
			return t
		}
	}
	if p, err := exec.LookPath("nvcc"); err == nil {
		t.NVCC, t.NVCCFrom = p, "PATH"
		return t
	}
	const conventional = "/usr/local/cuda/bin/nvcc"
	if _, err := os.Stat(conventional); err == nil {
		t.NVCC, t.NVCCFrom = conventional, "/usr/local/cuda"
	}
	return t
}

// hostCXXPackage is what provides g++ on the distributions install.sh
// supports with apt. Named once so the message and the command cannot
// drift apart.
const hostCXXPackage = "g++"

// ensureHostCXX installs g++ when it is missing and apt is available,
// and reports what happened in one line for the install log
// (waired-agent#898).
//
// Installing rather than only warning, because this is the same shape of
// action install.sh already takes — it apt-installs ca-certificates,
// curl and gnupg as prerequisites — and because a compiler is not a
// preference the operator chose, it is a thing vLLM cannot run without.
// The CUDA toolkit deliberately stays a warning: it is a multi-GB
// download whose version has to match the torch build, which is a
// decision rather than a prerequisite.
//
// apt only. On anything else this reports what to install and returns
// without acting — guessing a package manager is how a fix reaches for
// a machine it was never meant to touch.
func ensureHostCXX(ctx context.Context, run func(ctx context.Context, name string, args ...string) error) (advisory string, installed bool) {
	if _, err := exec.LookPath("g++"); err == nil {
		return "", false
	}
	apt, err := exec.LookPath("apt-get")
	if err != nil {
		return "no C++ compiler (" + hostCXXPackage + ") and no apt to install one with; " +
			"install " + hostCXXPackage + " with this system's package manager or the engine will not start", false
	}
	argv := []string{apt, "install", "-y", hostCXXPackage}
	if os.Geteuid() != 0 {
		sudo, serr := exec.LookPath("sudo")
		if serr != nil {
			return "no C++ compiler (" + hostCXXPackage + "); installing one needs root and sudo is not " +
				"available. Run: apt-get install -y " + hostCXXPackage, false
		}
		argv = append([]string{sudo}, argv...)
	}
	if err := run(ctx, argv[0], argv[1:]...); err != nil {
		return "could not install " + hostCXXPackage + " (" + err.Error() + "); the engine will not " +
			"start until it is present. Run: apt-get install -y " + hostCXXPackage, false
	}
	if _, err := exec.LookPath("g++"); err != nil {
		return "apt reported success but " + hostCXXPackage + " is still not on PATH; " +
			"the engine will not start until it is", false
	}
	return "", true
}

// runAptInstall is ensureHostCXX's default runner, kept apart so tests
// can drive the decision without a package manager.
func runAptInstall(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	// sudo may need to prompt, exactly as the tray's appindicator
	// repair does (internal/platform/trayhost/repair_linux.go).
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// nvccVersionOutput runs `<nvcc> --version` for readBundledCUDA. Errors
// yield "" so an unreadable bundle reports nothing rather than a
// disagreement it did not observe.
func nvccVersionOutput(nvcc string) string {
	out, err := exec.Command(nvcc, "--version").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
