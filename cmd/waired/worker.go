package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// runWorker dispatches `waired worker <get|set>`. Tailscale-exit-node-
// style manual selection of where outbound inference flows. Pin
// targets accept the operator-visible peer name (preferred) OR a
// DeviceID; names are resolved against /waired/v1/inference/mesh.
const workerLong = `Manual inference routing target (Tailscale-exit-node-style):

  waired worker get
  waired worker set --mode=auto|local-only|peer-preferred
  waired worker set --pin=<peer-name-or-device-id>

Choose where outbound inference flows. Persisted across daemon restarts.`

func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Manual inference routing target (Tailscale-exit-node-style): get / set.",
		Long:  workerLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newWorkerGetCmd(), newWorkerSetCmd())
	return cmd
}

// newWorkerGetCmd renders the current routing state in a human-readable form.
func newWorkerGetCmd() *cobra.Command {
	var mgmt string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Show the current routing mode and pinned peer.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := httpGet(workerURL(mgmt))
			if err != nil {
				return fmt.Errorf("waired worker get: %w", err)
			}
			if jsonOut {
				return prettyPrint(body)
			}
			var resp management.WorkerResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				return fmt.Errorf("waired worker get: parse: %w", err)
			}
			printWorkerResponse(os.Stdout, resp)
			return nil
		},
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output the raw WorkerResponse as JSON")
	return cmd
}

// newWorkerSetCmd POSTs the new mode/pin to the daemon. Pin lookups
// resolve a peer name to a DeviceID via /waired/v1/inference/mesh
// before the POST — the daemon accepts only DeviceID, so resolution
// here keeps the CLI argument format friendly without growing the
// management API surface.
func newWorkerSetCmd() *cobra.Command {
	var mgmt, mode, pin string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set the routing mode (--mode) or pin a peer (--pin).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := buildWorkerRequest(mgmt, mode, pin)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("waired worker set: encode: %w", err)
			}
			body, err := httpPost(workerURL(mgmt), payload)
			if err != nil {
				return fmt.Errorf("waired worker set: %w", err)
			}
			var resp management.WorkerResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				return fmt.Errorf("waired worker set: parse: %w", err)
			}
			printWorkerResponse(os.Stdout, resp)
			return nil
		},
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	cmd.Flags().StringVar(&mode, "mode", "", "routing mode: auto|local-only|peer-preferred|pinned")
	cmd.Flags().StringVar(&pin, "pin", "", "peer name or DeviceID to pin (implies --mode=pinned)")
	return cmd
}

// buildWorkerRequest validates the flag pair and resolves a peer name
// to a DeviceID if necessary.
//
// Rules:
//
//   - --mode and --pin both empty → error (no-op)
//   - --pin without --mode        → mode=pinned
//   - --mode=pinned without --pin → error (handler would reject anyway)
//   - --mode set + --pin set      → mode must be "pinned" or "" (we'll
//     normalise to "pinned") — any other mode + a pin is a user typo,
//     reject it locally instead of letting the daemon 400.
func buildWorkerRequest(mgmt, mode, pin string) (management.WorkerRequest, error) {
	if mode == "" && pin == "" {
		return management.WorkerRequest{}, fmt.Errorf("waired worker set: pass --mode or --pin")
	}
	if pin != "" {
		switch mode {
		case "", "pinned":
			// ok — auto-promote to pinned.
		default:
			return management.WorkerRequest{}, fmt.Errorf(
				"waired worker set: --pin is only valid with --mode=pinned (or no --mode); got --mode=%q", mode)
		}
		deviceID, err := resolvePeerToDeviceID(mgmt, pin)
		if err != nil {
			return management.WorkerRequest{}, err
		}
		return management.WorkerRequest{
			Mode:               state.RoutingModePinned,
			PinnedPeerDeviceID: deviceID,
		}, nil
	}
	// pin == ""
	switch state.RoutingMode(mode) {
	case state.RoutingModeAuto, state.RoutingModeLocalOnly, state.RoutingModePeerPreferred:
		return management.WorkerRequest{Mode: state.RoutingMode(mode)}, nil
	case state.RoutingModePinned:
		return management.WorkerRequest{}, fmt.Errorf(
			"waired worker set: --mode=pinned requires --pin=<peer>")
	default:
		return management.WorkerRequest{}, fmt.Errorf(
			"waired worker set: unknown --mode %q (want auto|local-only|peer-preferred)", mode)
	}
}

// resolvePeerToDeviceID accepts either a DeviceID (returned verbatim
// when an exact match is found in the snapshot) or a peer name, and
// returns the canonical DeviceID. Names are matched case-sensitively;
// ambiguous names (two peers with the same name) are rejected so the
// operator must use the DeviceID.
func resolvePeerToDeviceID(mgmt, nameOrID string) (string, error) {
	// fetchMeshSnapshot expects host:port form (no scheme). When the
	// caller passed --mgmt as a URL we strip the scheme so the helper's
	// internal "http://"+addr concatenation does not produce
	// "http://http://...".
	snap, err := fetchMeshSnapshot(meshAddrFromURL(mgmt), 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("waired worker set: resolve %q: %w", nameOrID, err)
	}
	var idMatch *inferencemesh.PeerView
	var nameMatches []inferencemesh.PeerView
	for i := range snap.Peers {
		p := &snap.Peers[i]
		if p.DeviceID == nameOrID {
			idMatch = p
		}
		if p.DeviceName != "" && p.DeviceName == nameOrID {
			nameMatches = append(nameMatches, *p)
		}
	}
	if idMatch != nil {
		return idMatch.DeviceID, nil
	}
	switch len(nameMatches) {
	case 0:
		return "", fmt.Errorf("waired worker set: peer %q not found in current mesh snapshot — run `waired peers list` to see available peers", nameOrID)
	case 1:
		return nameMatches[0].DeviceID, nil
	default:
		ids := make([]string, 0, len(nameMatches))
		for _, p := range nameMatches {
			ids = append(ids, p.DeviceID)
		}
		return "", fmt.Errorf(
			"waired worker set: peer name %q is ambiguous — %d peers share it (DeviceIDs: %s). Use the DeviceID instead",
			nameOrID, len(nameMatches), strings.Join(ids, ", "))
	}
}

func workerURL(mgmt string) string {
	mgmt = strings.TrimRight(mgmt, "/")
	if !strings.HasPrefix(mgmt, "http://") && !strings.HasPrefix(mgmt, "https://") {
		mgmt = "http://" + mgmt
	}
	return mgmt + "/waired/v1/worker"
}

// meshAddrFromURL strips an http(s):// scheme + trailing slashes so the
// resulting addr fits fetchMeshSnapshot's host:port contract.
func meshAddrFromURL(mgmt string) string {
	mgmt = strings.TrimRight(mgmt, "/")
	mgmt = strings.TrimPrefix(mgmt, "http://")
	mgmt = strings.TrimPrefix(mgmt, "https://")
	return mgmt
}

func printWorkerResponse(w *os.File, resp management.WorkerResponse) {
	out := &bytes.Buffer{}
	fmt.Fprintf(out, "mode:        %s\n", displayMode(resp.Mode))
	if resp.Mode == state.RoutingModePinned {
		fmt.Fprintf(out, "worker:      %s", displayPin(resp))
		fmt.Fprintln(out)
		fmt.Fprintf(out, "status:      %s\n", displayPinStatus(resp.PinnedPeerStatus))
	}
	_, _ = w.Write(out.Bytes())
}

func displayMode(m state.RoutingMode) string {
	if m == "" {
		return string(state.RoutingModeAuto)
	}
	return string(m)
}

func displayPin(resp management.WorkerResponse) string {
	if resp.PinnedPeerName != "" {
		return fmt.Sprintf("%s (%s)", resp.PinnedPeerName, resp.PinnedPeerDeviceID)
	}
	return resp.PinnedPeerDeviceID
}

func displayPinStatus(s string) string {
	switch s {
	case "ok":
		return "ok (peer reachable, serving)"
	case "unavailable":
		return "unavailable (peer present but not serving)"
	case "absent":
		return "absent (peer not in current mesh snapshot)"
	case "":
		return "unknown"
	default:
		return s
	}
}
