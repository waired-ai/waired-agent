package main

import "github.com/waired-ai/waired-agent/internal/management"

// nodeKeyAgreement compares the Node Key the control plane publishes for
// this device (from the signed network map's self row) against the
// public half of the key the device is actually running on.
//
// Pure so the three outcomes are testable without a control plane: the
// one that matters is only reachable on a host whose key has already
// gone stale, which is exactly the state nobody can reproduce on demand
// (waired-ai/waired#1137).
//
//   - published == local                  → agreed.
//   - published != local == publishedPrev → a rotation the control plane
//     has accepted and this agent has not yet promoted. Transient by
//     design, and the map publishes the outgoing key for this window.
//   - published != local != publishedPrev → diverged: peers are
//     authenticating against a key this device does not hold.
//
// An empty published key means the map carries no self Node Key at all
// (a control plane predating the field), which is unknown, not a
// mismatch — the caller leaves the verdict empty.
func nodeKeyAgreement(local, published, publishedPrev string) string {
	if local == "" || published == "" {
		return ""
	}
	switch local {
	case published:
		return management.NodeKeyAgreementOK
	case publishedPrev:
		return management.NodeKeyAgreementRotating
	default:
		return management.NodeKeyAgreementDiverged
	}
}
