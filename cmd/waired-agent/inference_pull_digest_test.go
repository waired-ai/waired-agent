package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// swapTagDigest installs a fake registry digest for one test.
func swapTagDigest(t *testing.T, digest string, err error) {
	t.Helper()
	prev := tagDigestFn
	tagDigestFn = func(context.Context, string) (string, error) { return digest, err }
	t.Cleanup(func() { tagDigestFn = prev })
}

const (
	pinnedDigest = "sha256:25c2189c92da5e52d684bd867de53557ed1572a5da8789f61e24df89b411d154"
	movedDigest  = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

func pinnedManifest() catalog.Manifest {
	m := pullGateManifest(false)
	m.Variants = m.Variants[1:] // the unfloored q4, so the pull takes it
	m.Variants[0].Source.Digest = pinnedDigest
	return m
}

// PRODUCT CONTRACT (waired-agent#1305): a pinned tag that the registry now
// serves other weights under is not downloaded. The catalog's size, fit
// and download time all describe the build it pinned; fetching a different
// one serves weights nobody measured on a host that was told they fit.
// The refusal is recorded on the row, and it is not retried.
func TestRunPullJob_RefusesAPinnedTagWhoseBuildMoved(t *testing.T) {
	swapTagDigest(t, movedDigest, nil)
	r := &scriptedRunner{results: []error{nil}}
	p := pullGateProviderWithRunner(t, pinnedManifest(), r)

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.calls(); got != 0 {
		t.Errorf("ollama pull ran %d time(s) for a tag whose build moved; want 0", got)
	}
	ms := modelStateOf(t, p, "dense-mtp")
	if ms.State != catalog.ModelStateFailed {
		t.Errorf("state = %q, want failed", ms.State)
	}
	if !strings.Contains(ms.Error, errSourceChanged.Error()) || !strings.Contains(ms.Error, movedDigest) {
		t.Errorf("recorded failure %q does not say the build moved, or to what", ms.Error)
	}
}

// The pin protects a recorded build; it must not stop a pull that could
// only have been checked, not refused: the digest matches, or the
// registry could not be asked.
func TestRunPullJob_PinnedTagPullsWhenTheDigestMatchesOrCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		digest string
		err    error
	}{
		{"digest matches", pinnedDigest, nil},
		{"registry unreachable", "", errors.New("dial tcp: no route to host")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapTagDigest(t, tc.digest, tc.err)
			r := &scriptedRunner{results: []error{nil}}
			p := pullGateProviderWithRunner(t, pinnedManifest(), r)
			if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
				t.Fatalf("PullModel: %v", err)
			}
			p.waitForPulls()
			if got := r.calls(); got != 1 {
				t.Errorf("ollama pull ran %d time(s); want 1", got)
			}
			if ms := modelStateOf(t, p, "dense-mtp"); ms.State != catalog.ModelStateReady {
				t.Errorf("state = %q, want ready (error %q)", ms.State, ms.Error)
			}
		})
	}
}
