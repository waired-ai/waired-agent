package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/waired-ai/waired-agent/internal/setup"
)

// bundleSignatureReport is the raw result of asking macOS whether an app
// bundle is intact and runnable. Gathering it shells out and is therefore
// per-OS; judging it is pure and lives here so the judgement table-tests on
// every runner.
type bundleSignatureReport struct {
	// Path is the bundle that was assessed, for the error text.
	Path string
	// CodesignOut / CodesignErr: `codesign --verify --deep --strict`.
	// This is the check that catches #329's corruption: it reports
	// "unsealed contents present in the bundle root".
	CodesignOut string
	CodesignErr error
	// SpctlOut / SpctlErr: `spctl --assess --type execute`. Gatekeeper's own
	// verdict on whether it will let the thing run.
	SpctlOut string
	SpctlErr error
	// Probed is false when the check could not be made at all (not macOS, no
	// bundle at that path). An unprobed bundle is never called broken —
	// "we did not look" must not read as "it is fine" OR as "it is broken".
	Probed bool
}

// bundleSignatureVerdict turns a report into an error, or nil when the bundle
// is intact.
//
// Both tools are static: they read the bundle and its signature and never
// execute the binary inside it, so neither can raise the Gatekeeper "damaged"
// dialog. That is why validation is done this way rather than by running
// `ollama --version` — one exec of a broken install costs the user a dialog.
//
// The tools' own output is carried into the error verbatim, because it names
// the actual defect ("unsealed contents present in the bundle root") and that
// text is what reaches the setup wizard's engine row.
func bundleSignatureVerdict(r bundleSignatureReport) error {
	if !r.Probed {
		return nil
	}
	var problems []string
	if r.CodesignErr != nil {
		problems = append(problems, "codesign: "+signatureDetail(r.CodesignOut, r.CodesignErr))
	}
	if r.SpctlErr != nil {
		problems = append(problems, "spctl: "+signatureDetail(r.SpctlOut, r.SpctlErr))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: %s (%s) — so every attempt to start the AI engine is killed",
		errEngineBundleSignature, r.Path, strings.Join(problems, "; "))
}

// signatureDetail prefers the tool's own message and falls back to the exit
// error when it printed nothing.
func signatureDetail(out string, err error) string {
	if s := strings.TrimSpace(out); s != "" {
		// Both tools print one line per problem; keep it to the first two so a
		// --deep walk over a large bundle cannot flood the wizard's error row.
		lines := strings.Split(s, "\n")
		if len(lines) > 2 {
			lines = lines[:2]
		}
		return strings.Join(lines, "; ")
	}
	if err != nil {
		return err.Error()
	}
	return "rejected"
}

// errEngineBundleSignature marks a failure that is a VERDICT rather than a
// guess: macOS was asked directly and said it will not run this bundle. The
// setup executor keys the wire error code off it, so the wizard's engine row
// says "the engine cannot start" instead of the catch-all "check your internet
// connection".
var errEngineBundleSignature = errors.New("macOS will not run the installed AI engine")

// isBundleSignatureError reports whether err came from that verdict.
func isBundleSignatureError(err error) bool { return errors.Is(err, errEngineBundleSignature) }

// engineSignatureBroken is the boolean form the install decision needs.
// Untagged so the two per-OS halves stay a single, small difference:
// engineBundleSignatureProblem.
func engineSignatureBroken(ctx context.Context, det setup.OllamaDetection) bool {
	return engineBundleSignatureProblem(ctx, det) != nil
}
