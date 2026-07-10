//go:build testharness && linux

package testharness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// recordingIPTabler captures every Run() invocation. Unprimed args
// default to (output="", exitCode=0, err=nil) — i.e., "succeeded".
// prime() lets a test pin a specific args slice to a non-zero exit
// code or a system error.
type recordingIPTabler struct {
	mu     sync.Mutex
	calls  [][]string
	primed map[string]ipResp
}

type ipResp struct {
	output   string
	exitCode int
	err      error
}

func newRecordingIPT() *recordingIPTabler {
	return &recordingIPTabler{primed: map[string]ipResp{}}
}

func (r *recordingIPTabler) prime(args []string, exitCode int, err error) {
	r.primed[strings.Join(args, " ")] = ipResp{exitCode: exitCode, err: err}
}

func (r *recordingIPTabler) Run(_ context.Context, args ...string) (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	if resp, ok := r.primed[strings.Join(args, " ")]; ok {
		return resp.output, resp.exitCode, resp.err
	}
	// Sensible defaults for unprimed calls:
	//   -C → 1 (rule not present): a fresh tabletop has no installed
	//          rules, so the precheck must report "absent" by default.
	//          Tests that want "already present" can prime to 0.
	//   All other verbs → 0 (success), which matches iptables' real
	//          behaviour for -N/-I/-A/-D/-F/-X on a clean slate or a
	//          chain already in the desired state.
	if len(args) > 0 && args[0] == "-C" {
		return "", 1, nil
	}
	return "", 0, nil
}

func (r *recordingIPTabler) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i := range r.calls {
		out[i] = append([]string(nil), r.calls[i]...)
	}
	return out
}

func (r *recordingIPTabler) hasCall(args []string) bool {
	target := strings.Join(args, " ")
	for _, c := range r.snapshot() {
		if strings.Join(c, " ") == target {
			return true
		}
	}
	return false
}

func TestInstallChain_FreshHost(t *testing.T) {
	// Fresh host: -N exits 0 (created), -C INPUT/OUTPUT exits 1
	// (not yet installed) → installer must spawn -I for both hooks.
	ipt := newRecordingIPT()
	ipt.prime([]string{"-C", "INPUT", "-j", IPTablesChain}, 1, nil)
	ipt.prime([]string{"-C", "OUTPUT", "-j", IPTablesChain}, 1, nil)

	if err := InstallChain(context.Background(), ipt); err != nil {
		t.Fatalf("InstallChain: %v", err)
	}
	expected := [][]string{
		{"-N", IPTablesChain},
		{"-C", "INPUT", "-j", IPTablesChain},
		{"-I", "INPUT", "1", "-j", IPTablesChain},
		{"-C", "OUTPUT", "-j", IPTablesChain},
		{"-I", "OUTPUT", "1", "-j", IPTablesChain},
	}
	for _, c := range expected {
		if !ipt.hasCall(c) {
			t.Errorf("missing call: %v\nall calls: %v", c, ipt.snapshot())
		}
	}
}

func TestInstallChain_AlreadyInstalled(t *testing.T) {
	// -N exits 1 (chain exists), -C exits 0 (jump present) →
	// installer must not call -I.
	ipt := newRecordingIPT()
	ipt.prime([]string{"-N", IPTablesChain}, 1, nil)
	// -C defaults to 1 (rule absent); prime the jump prechecks
	// to 0 to assert the "already installed" branch.
	ipt.prime([]string{"-C", "INPUT", "-j", IPTablesChain}, 0, nil)
	ipt.prime([]string{"-C", "OUTPUT", "-j", IPTablesChain}, 0, nil)

	if err := InstallChain(context.Background(), ipt); err != nil {
		t.Fatalf("InstallChain: %v", err)
	}
	for _, c := range [][]string{
		{"-I", "INPUT", "1", "-j", IPTablesChain},
		{"-I", "OUTPUT", "1", "-j", IPTablesChain},
	} {
		if ipt.hasCall(c) {
			t.Errorf("unexpected -I call (should be idempotent): %v", c)
		}
	}
}

func TestInstallChain_SystemErrorPropagates(t *testing.T) {
	ipt := newRecordingIPT()
	ipt.prime([]string{"-N", IPTablesChain}, -1, errors.New("iptables: command not found"))
	if err := InstallChain(context.Background(), ipt); err == nil {
		t.Fatalf("expected error from missing iptables binary")
	}
}

func TestInstallChain_UnexpectedExitCodeIsError(t *testing.T) {
	ipt := newRecordingIPT()
	ipt.prime([]string{"-N", IPTablesChain}, 2, nil) // 2 = bad arg
	if err := InstallChain(context.Background(), ipt); err == nil {
		t.Fatalf("expected error for unexpected -N exit code")
	}
}

func TestBlockUDPDirect_Both(t *testing.T) {
	ipt := newRecordingIPT()
	// Fresh chain: -C exits 1 (rule absent) so the -A must follow.
	for _, ip := range []string{"1.2.3.4", "5.6.7.8"} {
		ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", ip, "-j", "DROP"}, 1, nil)
		ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", ip, "-j", "DROP"}, 1, nil)
	}
	if err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"1.2.3.4", "5.6.7.8"}, signer.ScenarioDirectionBoth); err != nil {
		t.Fatalf("BlockUDPDirect: %v", err)
	}
	expected := [][]string{
		{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"},
		{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"},
		{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "5.6.7.8", "-j", "DROP"},
		{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "5.6.7.8", "-j", "DROP"},
	}
	for _, c := range expected {
		if !ipt.hasCall(c) {
			t.Errorf("missing call: %v", c)
		}
	}
}

// TestBlockUDPDirect_Idempotent checks that calling BlockUDPDirect
// twice with overlapping IPs emits at most one -A per (IP, direction)
// — the second call's -C precheck reports the rule already exists
// (exit 0) and the function skips re-appending.
func TestBlockUDPDirect_Idempotent(t *testing.T) {
	ipt := newRecordingIPT()
	// First call: rules absent.
	ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"}, 1, nil)
	ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"}, 1, nil)
	if err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"1.2.3.4"}, signer.ScenarioDirectionBoth); err != nil {
		t.Fatalf("BlockUDPDirect (first): %v", err)
	}
	// Second call: simulate the rules now exist by priming -C → 0.
	ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"}, 0, nil)
	ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"}, 0, nil)
	if err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"1.2.3.4"}, signer.ScenarioDirectionBoth); err != nil {
		t.Fatalf("BlockUDPDirect (second): %v", err)
	}
	// Count -A invocations for 1.2.3.4 across both calls — must be
	// exactly 2 (one per direction), not 4.
	var nAppend int
	for _, c := range ipt.snapshot() {
		if len(c) > 1 && c[0] == "-A" && strings.Contains(strings.Join(c, " "), "1.2.3.4") {
			nAppend++
		}
	}
	if nAppend != 2 {
		t.Errorf("-A calls for 1.2.3.4 = %d, want 2 (idempotency violated)\nall calls: %v", nAppend, ipt.snapshot())
	}
}

func TestBlockUDPDirect_OutboundOnly(t *testing.T) {
	ipt := newRecordingIPT()
	if err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"1.2.3.4"}, signer.ScenarioDirectionOutbound); err != nil {
		t.Fatalf("BlockUDPDirect: %v", err)
	}
	if ipt.hasCall([]string{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"}) {
		t.Errorf("inbound (-s) rule emitted in outbound-only mode")
	}
	if !ipt.hasCall([]string{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"}) {
		t.Errorf("outbound (-d) rule missing")
	}
}

func TestBlockUDPDirect_InboundOnly(t *testing.T) {
	ipt := newRecordingIPT()
	if err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"1.2.3.4"}, signer.ScenarioDirectionInbound); err != nil {
		t.Fatalf("BlockUDPDirect: %v", err)
	}
	if ipt.hasCall([]string{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"}) {
		t.Errorf("outbound (-d) rule emitted in inbound-only mode")
	}
	if !ipt.hasCall([]string{"-A", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"}) {
		t.Errorf("inbound (-s) rule missing")
	}
}

func TestBlockUDPDirect_SkipsIPv6Silently(t *testing.T) {
	ipt := newRecordingIPT()
	ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-s", "1.2.3.4", "-j", "DROP"}, 1, nil)
	ipt.prime([]string{"-C", IPTablesChain, "-p", "udp", "--dport", "51820", "-d", "1.2.3.4", "-j", "DROP"}, 1, nil)
	if err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"2001:db8::1", "1.2.3.4"}, signer.ScenarioDirectionBoth); err != nil {
		t.Fatalf("BlockUDPDirect: %v", err)
	}
	// Count -A appends only (idempotency precheck -C calls don't count
	// as "installed rules").
	var ipv4Rules, ipv6Rules int
	for _, c := range ipt.snapshot() {
		if len(c) == 0 || c[0] != "-A" {
			continue
		}
		joined := strings.Join(c, " ")
		switch {
		case strings.Contains(joined, "1.2.3.4"):
			ipv4Rules++
		case strings.Contains(joined, "2001:db8") || strings.Contains(joined, "[2001"):
			ipv6Rules++
		}
	}
	if ipv4Rules != 2 {
		t.Errorf("ipv4 -A rules: %d want 2", ipv4Rules)
	}
	if ipv6Rules != 0 {
		t.Errorf("ipv6 -A rules: %d want 0 (must be silently skipped)", ipv6Rules)
	}
}

func TestBlockUDPDirect_InvalidDirection(t *testing.T) {
	ipt := newRecordingIPT()
	err := BlockUDPDirect(context.Background(), ipt, 51820, []string{"1.2.3.4"}, "sideways")
	if err == nil {
		t.Fatalf("expected error for invalid direction")
	}
}

func TestBlockUDPDirect_InvalidIPSkippedNotError(t *testing.T) {
	ipt := newRecordingIPT()
	err := BlockUDPDirect(context.Background(), ipt, 51820,
		[]string{"not-an-ip", "1.2.3.4"}, signer.ScenarioDirectionBoth)
	if err != nil {
		t.Fatalf("BlockUDPDirect should skip invalid IPs not error: %v", err)
	}
	for _, c := range ipt.snapshot() {
		if strings.Contains(strings.Join(c, " "), "not-an-ip") {
			t.Errorf("invalid IP made it into iptables call: %v", c)
		}
	}
}

func TestFlushChain_HandlesAlreadyClean(t *testing.T) {
	// -D / -F / -X all exit 1 when the rule/chain does not exist;
	// FlushChain must absorb this.
	ipt := newRecordingIPT()
	ipt.prime([]string{"-D", "INPUT", "-j", IPTablesChain}, 1, nil)
	ipt.prime([]string{"-D", "OUTPUT", "-j", IPTablesChain}, 1, nil)
	ipt.prime([]string{"-F", IPTablesChain}, 1, nil)
	ipt.prime([]string{"-X", IPTablesChain}, 1, nil)
	if err := FlushChain(context.Background(), ipt); err != nil {
		t.Errorf("FlushChain (already clean): %v", err)
	}
}

func TestFlushChain_FullCleanup(t *testing.T) {
	ipt := newRecordingIPT()
	if err := FlushChain(context.Background(), ipt); err != nil {
		t.Fatalf("FlushChain: %v", err)
	}
	expected := [][]string{
		{"-D", "INPUT", "-j", IPTablesChain},
		{"-D", "OUTPUT", "-j", IPTablesChain},
		{"-F", IPTablesChain},
		{"-X", IPTablesChain},
	}
	for _, c := range expected {
		if !ipt.hasCall(c) {
			t.Errorf("missing call: %v", c)
		}
	}
}

func TestFlushChain_PropagatesSystemError(t *testing.T) {
	ipt := newRecordingIPT()
	ipt.prime([]string{"-D", "INPUT", "-j", IPTablesChain}, -1, errors.New("iptables: command not found"))
	if err := FlushChain(context.Background(), ipt); err == nil {
		t.Fatalf("expected error from missing iptables binary")
	}
}
