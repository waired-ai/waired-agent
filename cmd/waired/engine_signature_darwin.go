//go:build darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/waired-ai/waired-agent/internal/setup"
)

// checkBundleSignature asks macOS whether the app bundle at appPath is intact
// and runnable. Both tools are static — neither executes the binary inside the
// bundle — so this is safe to run against an install that Gatekeeper would
// kill on sight.
//
// A missing bundle yields Probed=false rather than an error: "there is no app
// bundle here" is the Homebrew / bare-CLI layout, not a corruption.
var checkBundleSignature = func(ctx context.Context, appPath string) bundleSignatureReport {
	r := bundleSignatureReport{Path: appPath}
	if appPath == "" {
		return r
	}
	if fi, err := os.Stat(appPath); err != nil || !fi.IsDir() {
		return r
	}
	r.Probed = true
	r.CodesignOut, r.CodesignErr = runDarwinCmdOutput(ctx, "codesign", "--verify", "--deep", "--strict", appPath)
	r.SpctlOut, r.SpctlErr = runDarwinCmdOutput(ctx, "spctl", "--assess", "--type", "execute", appPath)
	return r
}

// engineBundlePath maps a resolved engine binary to the app bundle that
// contains it, or "" for a plain-directory install.
func engineBundlePath(binPath string) string {
	for d := filepath.Dir(binPath); ; {
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
		if strings.HasSuffix(strings.ToLower(d), ".app") {
			return d
		}
	}
}

// engineBundleSignatureProblem reports why macOS will not run the engine at
// det.Path, or nil when it will.
//
// Deliberately conservative: anything it could not assess is NOT a problem. A
// false positive here re-downloads a perfectly good engine, and tells a user
// with a working install that it is broken.
func engineBundleSignatureProblem(ctx context.Context, det setup.OllamaDetection) error {
	if !det.Installed {
		return nil
	}
	app := engineBundlePath(det.Path)
	if app == "" {
		// A bare CLI (Homebrew, /usr/local/bin) has no bundle and no seal.
		return nil
	}
	return bundleSignatureVerdict(checkBundleSignature(ctx, app))
}
