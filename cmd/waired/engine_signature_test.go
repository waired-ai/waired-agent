package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// The real codesign/spctl output this is built to read. Kept verbatim from a
// host that carried the #329 defect, because the whole point of the verdict is
// that the tool's own words reach the user.
const (
	realCodesignRejection = "/Applications/Ollama.app: unsealed contents present in the bundle root"
	realSpctlRejection    = "/Applications/Ollama.app: rejected (unsealed contents present in the bundle root)\n" +
		"origin=Developer ID Application: Infra Technologies, Inc"
)

// TestBundleSignatureVerdict is a PRODUCT CONTRACT test.
//
// The load-bearing row is the last one: an UNPROBED bundle is never called
// broken. "We could not look" must read as neither "it is fine" nor "it is
// broken" — treating it as broken would make every non-macOS host, and every
// Homebrew/bare-CLI macOS host, reinstall an engine that works.
func TestBundleSignatureVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		report   bundleSignatureReport
		wantErr  bool
		wantText []string
	}{
		{
			name: "both tools accept",
			report: bundleSignatureReport{
				Path: "/Applications/Ollama.app", Probed: true,
				CodesignOut: "/Applications/Ollama.app: valid on disk",
				SpctlOut:    "/Applications/Ollama.app: accepted",
			},
		},
		{
			name: "codesign rejects the broken seal",
			report: bundleSignatureReport{
				Path: "/Applications/Ollama.app", Probed: true,
				CodesignOut: realCodesignRejection,
				CodesignErr: errors.New("exit status 1"),
			},
			wantErr: true,
			// The tool's own diagnosis has to survive into the error: it is
			// what tells the user (and the wizard row) what is actually wrong.
			wantText: []string{"unsealed contents present in the bundle root", "/Applications/Ollama.app"},
		},
		{
			name: "spctl rejects",
			report: bundleSignatureReport{
				Path: "/Applications/Ollama.app", Probed: true,
				CodesignOut: "/Applications/Ollama.app: valid on disk",
				SpctlOut:    realSpctlRejection,
				SpctlErr:    errors.New("exit status 3"),
			},
			wantErr:  true,
			wantText: []string{"rejected"},
		},
		{
			name: "both reject",
			report: bundleSignatureReport{
				Path: "/Applications/Ollama.app", Probed: true,
				CodesignOut: realCodesignRejection, CodesignErr: errors.New("exit status 1"),
				SpctlOut: realSpctlRejection, SpctlErr: errors.New("exit status 3"),
			},
			wantErr:  true,
			wantText: []string{"codesign:", "spctl:"},
		},
		{
			name: "a tool that printed nothing still reports its exit error",
			report: bundleSignatureReport{
				Path: "/Applications/Ollama.app", Probed: true,
				CodesignErr: errors.New("exec: \"codesign\": executable file not found in $PATH"),
			},
			wantErr:  true,
			wantText: []string{"executable file not found"},
		},
		{
			// Not probed: not macOS, or no bundle at that path.
			name:   "unprobed is never broken",
			report: bundleSignatureReport{Path: "/opt/homebrew/bin/ollama"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := bundleSignatureVerdict(tc.report)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !isBundleSignatureError(err) {
				t.Error("the error must be identifiable as a signature verdict, not just prose")
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

// A --deep walk over a large bundle can print a line per nested problem; the
// detail lands in a wizard row, so it must not become a wall of text.
func TestBundleSignatureVerdict_ClampsFloodyOutput(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "nested/Frameworks/thing: code object is not signed at all")
	}
	err := bundleSignatureVerdict(bundleSignatureReport{
		Path: "/Applications/Ollama.app", Probed: true,
		CodesignOut: strings.Join(lines, "\n"), CodesignErr: errors.New("exit status 1"),
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if n := strings.Count(err.Error(), "code object is not signed"); n > 2 {
		t.Errorf("error repeats the same line %d times; want at most 2", n)
	}
}

// TestEngineInstallErrorCode is a PRODUCT CONTRACT test: a signature rejection
// is a verdict this process reached by asking macOS, so it declares its own
// code. Left undeclared it falls through the daemon's classifySetupFailure
// catch-all and gets painted "network_error" — sending the user to check their
// internet connection about a bundle that downloaded perfectly (#330).
func TestEngineInstallErrorCode(t *testing.T) {
	sigErr := bundleSignatureVerdict(bundleSignatureReport{
		Path: "/Applications/Ollama.app", Probed: true,
		CodesignOut: realCodesignRejection, CodesignErr: errors.New("exit status 1"),
	})
	if got := engineInstallErrorCode(sigErr); got != signer.SetupErrorEngineNotReady {
		t.Errorf("code for a signature rejection = %q, want %q", got, signer.SetupErrorEngineNotReady)
	}
	// Wrapped further up the stack (installOllama wraps with "ollama install: ")
	// it must still be recognised.
	if got := engineInstallErrorCode(errors.New("ollama install: " + sigErr.Error())); got != "" {
		t.Errorf("a merely string-formatted error must NOT be treated as a verdict, got %q", got)
	}
	if got := engineInstallErrorCode(wrapped{sigErr}); got != signer.SetupErrorEngineNotReady {
		t.Errorf("code through a wrapping error = %q, want %q", got, signer.SetupErrorEngineNotReady)
	}
	// Everything else stays undeclared so the daemon's text classification
	// keeps its disk-full / network reading.
	for _, err := range []error{
		errors.New("download failed: no space left on device"),
		errors.New("dial tcp: lookup github.com: no such host"),
	} {
		if got := engineInstallErrorCode(err); got != "" {
			t.Errorf("code for %v = %q, want empty (let the daemon classify)", err, got)
		}
	}
}

type wrapped struct{ err error }

func (w wrapped) Error() string { return "ollama install: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }
