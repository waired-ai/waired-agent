//go:build testharness && linux

package testharness

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// IPTablesChain is the custom chain the test-harness owns. Inserted at
// position 1 of INPUT and OUTPUT so the DROP rules land before any
// existing default-permit rule. The name is intentionally distinct
// from the legacy `WAIRED_FALLBACK_TEST` chain used by the (now
// removed) bash scripts so a host that previously ran the bash flow
// can keep both chains coexisting if needed.
const IPTablesChain = "WAIRED_TESTHARNESS"

// IPTabler abstracts an iptables process invocation. Tests inject a
// recordingIPTabler; production uses ExecIPTabler.
//
// Run reports the iptables exit code separately from the system-level
// error: a non-zero exitCode (e.g., 1 when `-C` reports "rule does not
// exist") is normal and surfaced as exitCode != 0; err is non-nil only
// for genuine system errors (binary not found, context cancelled).
type IPTabler interface {
	Run(ctx context.Context, args ...string) (output string, exitCode int, err error)
}

// ExecIPTabler shells out to /sbin/iptables (resolved via $PATH).
type ExecIPTabler struct{}

func (ExecIPTabler) Run(ctx context.Context, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "iptables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode(), nil
		}
		return string(out), -1, fmt.Errorf("iptables %v: %w", args, err)
	}
	return string(out), 0, nil
}

// InstallChain creates IPTablesChain (idempotent: existing chain is
// fine) and inserts a jump from INPUT and OUTPUT at position 1, again
// idempotent (existing jumps are kept in place).
func InstallChain(ctx context.Context, ipt IPTabler) error {
	// `-N` returns exit 1 if the chain already exists. Both 0 and 1
	// are acceptable; anything else (e.g., 2 = bad arg) is an error.
	if _, code, err := ipt.Run(ctx, "-N", IPTablesChain); err != nil {
		return err
	} else if code != 0 && code != 1 {
		return fmt.Errorf("iptables -N %s: exit %d", IPTablesChain, code)
	}
	for _, hook := range []string{"INPUT", "OUTPUT"} {
		_, code, err := ipt.Run(ctx, "-C", hook, "-j", IPTablesChain)
		if err != nil {
			return err
		}
		if code == 0 {
			continue // jump already present
		}
		if _, _, err := ipt.Run(ctx, "-I", hook, "1", "-j", IPTablesChain); err != nil {
			return err
		}
	}
	return nil
}

// BlockUDPDirect installs DROP rules in IPTablesChain for UDP/<port>
// traffic to or from the given peer IPs. Direction selects which
// chain hook the DROP applies to:
//
//	"inbound"  → -s peerIP   (drop packets coming FROM the peer)
//	"outbound" → -d peerIP   (drop packets going TO the peer)
//	"both"     → both of the above per peer
//
// IPv6 addresses are silently skipped — testnet today is IPv4-only,
// and `iptables` operates on IPv4 only (ip6tables would be the IPv6
// equivalent and is not yet wired here).
//
// Idempotent at the iptables layer: each `-A` is preceded by a `-C`
// existence check, so the second call with overlapping IPs adds no
// duplicate DROP rules. This lets the testharness dispatcher refresh
// the rule set when call_me_maybe surfaces new endpoint candidates
// without flushing and reinstalling the chain mid-scenario.
func BlockUDPDirect(ctx context.Context, ipt IPTabler, port int, peerIPs []string, direction string) error {
	switch direction {
	case signer.ScenarioDirectionBoth, signer.ScenarioDirectionInbound, signer.ScenarioDirectionOutbound:
	default:
		return fmt.Errorf("BlockUDPDirect: invalid direction %q", direction)
	}
	portStr := strconv.Itoa(port)
	for _, ip := range peerIPs {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() == nil {
			slog.Warn("testharness: skipping non-IPv4 peer endpoint", "ip", ip)
			continue
		}
		if direction == signer.ScenarioDirectionInbound || direction == signer.ScenarioDirectionBoth {
			if err := appendIfMissing(ctx, ipt, "-s", portStr, ip); err != nil {
				return err
			}
		}
		if direction == signer.ScenarioDirectionOutbound || direction == signer.ScenarioDirectionBoth {
			if err := appendIfMissing(ctx, ipt, "-d", portStr, ip); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendIfMissing installs a single DROP rule via `-A`, but only if a
// matching rule is not already present (checked via `-C`). selector is
// "-s" or "-d" — i.e., source-match for inbound rules, dest-match for
// outbound. Mirrors the chain-jump idempotency pattern in InstallChain.
func appendIfMissing(ctx context.Context, ipt IPTabler, selector, portStr, ip string) error {
	args := []string{IPTablesChain, "-p", "udp", "--dport", portStr, selector, ip, "-j", "DROP"}
	checkArgs := append([]string{"-C"}, args...)
	_, code, err := ipt.Run(ctx, checkArgs...)
	if err != nil {
		return err
	}
	if code == 0 {
		return nil // already present
	}
	appendArgs := append([]string{"-A"}, args...)
	if _, _, err := ipt.Run(ctx, appendArgs...); err != nil {
		return err
	}
	return nil
}

// FlushChain detaches the jump from INPUT and OUTPUT and then flushes
// + deletes IPTablesChain. Each step is best-effort: exit code 1
// ("rule/chain does not exist") is the desired end state and is
// absorbed silently. System errors (iptables binary missing, ctx
// cancelled) are surfaced.
func FlushChain(ctx context.Context, ipt IPTabler) error {
	for _, args := range [][]string{
		{"-D", "INPUT", "-j", IPTablesChain},
		{"-D", "OUTPUT", "-j", IPTablesChain},
		{"-F", IPTablesChain},
		{"-X", IPTablesChain},
	} {
		if _, _, err := ipt.Run(ctx, args...); err != nil {
			return fmt.Errorf("iptables %v: %w", args, err)
		}
	}
	return nil
}
