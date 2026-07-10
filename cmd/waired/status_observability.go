package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
)

// printObservabilitySection renders the agent's Phase 9 /state output
// at the bottom of `waired status --observability`. The mgmtURL is
// the same one printObservabilitySection's caller already used for
// /waired/v1/status (so the existing daemon-unreachable error has
// already been surfaced).
//
// format:
//
//	""     → human-readable text block (default)
//	"json" → raw JSON dump, intended for `jq` pipelines
//
// Any other value falls back to text with a stderr note so typos do
// not silently fail.
func printObservabilitySection(mgmtURL, format string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	state, err := observabilityclient.GetState(ctx, mgmtURL)
	if err != nil {
		fmt.Println()
		fmt.Println("Observability:")
		if errors.Is(err, observabilityclient.ErrUnsupported) {
			fmt.Println("  (daemon predates Phase 9 — upgrade waired-agent for fallback diagnostics)")
		} else {
			fmt.Println("  (unavailable:", err, ")")
		}
		return
	}

	switch format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(state)
	case "":
		printObservabilityText(*state)
	default:
		fmt.Fprintln(os.Stderr,
			"  (unknown --o format:", format, "— rendering as text)")
		printObservabilityText(*state)
	}
}

// printObservabilityText renders the state as a compact text block
// mirroring the layout the plan documents: one line per logical
// group (device / engine / mesh / last). Empty fields are elided
// rather than printed as "(empty)" so the block stays scannable on
// a pre-enrolment / fresh-install agent.
func printObservabilityText(s management.ObservabilityState) {
	fmt.Println()
	fmt.Println("Observability:")
	if s.Agent.DeviceID != "" {
		fmt.Printf("  Device:   %s  uptime: %s\n",
			s.Agent.DeviceID, humanUptime(s.Agent.UptimeSeconds))
	} else if s.Agent.UptimeSeconds > 0 {
		fmt.Printf("  Uptime:   %s\n", humanUptime(s.Agent.UptimeSeconds))
	}

	engine := "ready"
	if s.Agent.Paused {
		engine = "paused"
	} else if !s.Agent.EngineReady {
		engine = "not ready"
	}
	model := s.Agent.ModelID
	if model == "" {
		model = "(unknown)"
	}
	capStr := ""
	if s.Agent.CapacityTotal > 0 {
		capStr = fmt.Sprintf(", %d/%d slots used, inflight=%d",
			s.Agent.CapacityUsed, s.Agent.CapacityTotal, s.Agent.Inflight)
	}
	fmt.Printf("  Engine:   %s (model=%s%s)\n", engine, model, capStr)

	fmt.Printf("  Share:    %s   Paused: %s\n",
		boolEnabledDisabled(s.Agent.ShareEnabled), boolYesNo(s.Agent.Paused))

	fmt.Printf("  Mesh:     %d enrolled / %d reachable / %d ready\n",
		s.Mesh.PeersEnrolled, s.Mesh.PeersReachable, s.Mesh.PeersReady)

	if li := s.LastInference; li != nil {
		fb := "no"
		if li.HadFallback {
			fb = "yes"
		}
		peer := li.PeerID
		if peer == "" {
			peer = "(local)"
		}
		fmt.Printf("  Last:     %s  decision=%s  peer=%s  model=%s  latency=%dms  fallback=%s\n",
			li.TS, li.Decision, peer, li.Model, li.LatencyMs, fb)
	} else {
		fmt.Println("  Last:     (no inference yet)")
	}
}

func humanUptime(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		hrs := int(d.Hours()) - days*24
		return fmt.Sprintf("%dd%dh", days, hrs)
	}
}

func boolEnabledDisabled(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
