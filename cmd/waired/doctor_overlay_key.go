package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
)

// deviceKeyFinding reports whether the network is using the same key for
// this device that the device itself holds.
//
// The failure it exists for kills the overlay in both directions while
// every other surface still says the device is fine: peers reach it
// online, its own inference reports ready, and `waired doctor` passes.
// What actually happens is that peers seal handshakes to a key this
// device cannot answer with, and its own handshakes name a key no peer
// can match. On the rc8 hardware run that state produced 1302 rejected
// handshakes in 98 minutes with no surface naming the cause
// (waired-ai/waired#1137, waired-agent#712).
func deviceKeyFinding(ctx context.Context, mgmtURL string) integration.AuditFinding {
	cl := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mgmtURL+"/waired/v1/status", nil)
	if err != nil {
		return integration.AuditFinding{}
	}
	resp, err := cl.Do(req)
	if err != nil {
		// The daemon is unreachable; the management probe already
		// reports that with a better message than we could here.
		return integration.AuditFinding{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return integration.AuditFinding{}
	}
	var st management.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return integration.AuditFinding{}
	}
	return deviceKeyFindingFrom(st.NodeKeyAgreement)
}

// deviceKeyFindingFrom is the pure decision behind deviceKeyFinding.
//
// Agreement says nothing, deliberately. It is the normal state, and a
// line reading "device key ✓" tells a user who is not debugging the
// overlay nothing they can act on — the same reason the service check
// stays quiet when it has no history to report. An agent that has not
// received a network map yet, or a control plane that publishes no self
// key, is unknown rather than wrong, and is equally silent.
func deviceKeyFindingFrom(agreement string) integration.AuditFinding {
	switch agreement {
	case management.NodeKeyAgreementDiverged:
		return integration.AuditFinding{
			Status:  integration.StatusFail,
			Subject: "device key",
			Detail: "the network is using a different key for this computer than it holds. " +
				"No other computer can reach it over Waired. Run `waired init` to sign this computer in again.",
		}
	case management.NodeKeyAgreementRotating:
		return integration.AuditFinding{
			Status:  integration.StatusSkip,
			Subject: "device key",
			Detail:  "a key change is still finishing. Connections may retry for a moment, then settle",
		}
	default:
		return integration.AuditFinding{}
	}
}
