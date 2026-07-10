//go:build testharness && linux

package scenarios

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/testharness"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// recordingIPT mirrors the recordingIPTabler in iptables_linux_test.go.
// Duplicated here so the scenarios package's tests don't pull from a
// _test.go in the parent package (which would require build-tag
// gymnastics or exported test helpers). The two implementations should
// stay in sync — they're trivial mocks.
type recordingIPT struct {
	mu     sync.Mutex
	calls  [][]string
	primed map[string]ipResp
}

type ipResp struct {
	exitCode int
	err      error
}

func newRecordingIPT() *recordingIPT {
	return &recordingIPT{primed: map[string]ipResp{}}
}

func (r *recordingIPT) prime(args []string, exitCode int, err error) {
	r.primed[strings.Join(args, " ")] = ipResp{exitCode: exitCode, err: err}
}

func (r *recordingIPT) Run(_ context.Context, args ...string) (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	if resp, ok := r.primed[strings.Join(args, " ")]; ok {
		return "", resp.exitCode, resp.err
	}
	// -C defaults to "rule absent" (exit 1) so a fresh tabletop's
	// BlockUDPDirect actually emits the -A append. Mirrors the
	// recordingIPTabler in iptables_linux_test.go after the
	// idempotent-BlockUDPDirect rework.
	if len(args) > 0 && args[0] == "-C" {
		return "", 1, nil
	}
	return "", 0, nil
}

func (r *recordingIPT) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i := range r.calls {
		out[i] = append([]string(nil), r.calls[i]...)
	}
	return out
}

func (r *recordingIPT) hasCall(args []string) bool {
	target := strings.Join(args, " ")
	for _, c := range r.snapshot() {
		if strings.Join(c, " ") == target {
			return true
		}
	}
	return false
}

func (r *recordingIPT) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func paramsBoth(peerEndpoints ...string) testharness.ScenarioParams {
	return testharness.ScenarioParams{
		PeerDeviceID:  "dev_b",
		PeerEndpoints: peerEndpoints,
		Direction:     signer.ScenarioDirectionBoth,
		Nonce:         1,
	}
}

func TestFallbackBlocker_ApplyAndRevert(t *testing.T) {
	ipt := newRecordingIPT()
	sc := newFallbackBlocker(signer.ScenarioIDFallbackBasic, ipt, 51820)
	if sc.ID() != signer.ScenarioIDFallbackBasic {
		t.Fatalf("ID: %q", sc.ID())
	}
	if err := sc.Apply(context.Background(), paramsBoth("1.2.3.4")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range [][]string{
		{"-N", testharness.IPTablesChain},
		{"-A", testharness.IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"},
		{"-A", testharness.IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"},
	} {
		if !ipt.hasCall(c) {
			t.Errorf("missing call after Apply: %v", c)
		}
	}
	if err := sc.Revert(context.Background()); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	for _, c := range [][]string{
		{"-D", "INPUT", "-j", testharness.IPTablesChain},
		{"-D", "OUTPUT", "-j", testharness.IPTablesChain},
		{"-F", testharness.IPTablesChain},
		{"-X", testharness.IPTablesChain},
	} {
		if !ipt.hasCall(c) {
			t.Errorf("missing call after Revert: %v", c)
		}
	}
}

func TestFallbackBlocker_RevertWithoutApplyIsSafe(t *testing.T) {
	// Bash flow's unblock_direct equivalent: -D / -F / -X all return
	// exit 1 when nothing was installed. FlushChain (and so Revert)
	// absorb that quietly.
	ipt := newRecordingIPT()
	ipt.prime([]string{"-D", "INPUT", "-j", testharness.IPTablesChain}, 1, nil)
	ipt.prime([]string{"-D", "OUTPUT", "-j", testharness.IPTablesChain}, 1, nil)
	ipt.prime([]string{"-F", testharness.IPTablesChain}, 1, nil)
	ipt.prime([]string{"-X", testharness.IPTablesChain}, 1, nil)

	sc := newFallbackBlocker(signer.ScenarioIDFallbackBasic, ipt, 51820)
	if err := sc.Revert(context.Background()); err != nil {
		t.Errorf("Revert without Apply: %v", err)
	}
}

func TestFallbackBlocker_ApplyTwiceIsIdempotent(t *testing.T) {
	// The dispatcher already idempotency-gates by scenario+peer+nonce
	// + IP set, but the scenario itself must also tolerate being
	// invoked twice — Apply→Apply leaves the chain in the same end
	// state (rules might be duplicated but the effect is the same).
	ipt := newRecordingIPT()
	sc := newFallbackBlocker(signer.ScenarioIDFallbackBasic, ipt, 51820)
	if err := sc.Apply(context.Background(), paramsBoth("1.2.3.4")); err != nil {
		t.Fatalf("Apply 1: %v", err)
	}
	first := ipt.callCount()
	if err := sc.Apply(context.Background(), paramsBoth("1.2.3.4")); err != nil {
		t.Fatalf("Apply 2: %v", err)
	}
	second := ipt.callCount() - first
	if second == 0 {
		t.Errorf("second Apply did not invoke iptables — but the scenario layer should always re-run; idempotency lives in the dispatcher")
	}
}

func TestFallbackBlocker_AllThreeIDsRoundtripDistinctly(t *testing.T) {
	ipt := newRecordingIPT()
	for _, id := range []string{
		signer.ScenarioIDFallbackBasic,
		signer.ScenarioIDUpgradeBasic,
		signer.ScenarioIDFlapSuppression,
	} {
		sc := newFallbackBlocker(id, ipt, 51820)
		if sc.ID() != id {
			t.Errorf("id: got %q want %q", sc.ID(), id)
		}
	}
}

func TestAsymmetricBlocker_DirectionRouting(t *testing.T) {
	cases := []struct {
		name      string
		direction string
		wantSrc   bool
		wantDst   bool
	}{
		{"outbound", signer.ScenarioDirectionOutbound, false, true},
		{"inbound", signer.ScenarioDirectionInbound, true, false},
		{"both", signer.ScenarioDirectionBoth, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ipt := newRecordingIPT()
			sc := newAsymmetricBlocker(ipt, 51820)
			params := testharness.ScenarioParams{
				PeerDeviceID:  "dev_b",
				PeerEndpoints: []string{"1.2.3.4"},
				Direction:     c.direction,
				Nonce:         1,
			}
			if err := sc.Apply(context.Background(), params); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			srcRule := []string{"-A", testharness.IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"}
			dstRule := []string{"-A", testharness.IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"}
			if got := ipt.hasCall(srcRule); got != c.wantSrc {
				t.Errorf("src (-s) rule: got %v want %v", got, c.wantSrc)
			}
			if got := ipt.hasCall(dstRule); got != c.wantDst {
				t.Errorf("dst (-d) rule: got %v want %v", got, c.wantDst)
			}
		})
	}
}

func TestAsymmetricBlocker_ID(t *testing.T) {
	sc := newAsymmetricBlocker(newRecordingIPT(), 51820)
	if sc.ID() != signer.ScenarioIDAsymmetricDirect {
		t.Errorf("ID: %q", sc.ID())
	}
}

func TestDefaultRegistry_HasAllFourScenarios(t *testing.T) {
	reg := DefaultRegistry()
	for _, id := range []string{
		signer.ScenarioIDFallbackBasic,
		signer.ScenarioIDUpgradeBasic,
		signer.ScenarioIDFlapSuppression,
		signer.ScenarioIDAsymmetricDirect,
	} {
		sc, ok := reg[id]
		if !ok {
			t.Errorf("missing scenario %q", id)
			continue
		}
		if sc.ID() != id {
			t.Errorf("scenario %q: ID() returned %q", id, sc.ID())
		}
	}
	if len(reg) != 4 {
		t.Errorf("registry size: %d want 4", len(reg))
	}
}
