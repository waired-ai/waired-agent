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
	"github.com/waired-ai/waired-agent/proto/signer"
)

// runWorker dispatches `waired worker <get|set>`. Tailscale-exit-node-
// style manual selection of where outbound inference flows. Pin
// targets accept the operator-visible peer name (preferred) OR a
// DeviceID; names are resolved against /waired/v1/inference/mesh.
const workerLong = `Manual inference routing target (Tailscale-exit-node-style):

  waired worker get
  waired worker set --mode=auto|local-only|peer-preferred|peer-only
  waired worker set --pin=<peer-name-or-device-id>

Choose where outbound inference flows. Persisted across daemon restarts.

peer-preferred tries another computer first and falls back to this one;
peer-only never falls back — requests fail while no peer can serve them.`

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
	cmd.Flags().StringVar(&mode, "mode", "", "routing mode: auto|local-only|peer-preferred|peer-only|pinned")
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
	case state.RoutingModeAuto, state.RoutingModeLocalOnly,
		state.RoutingModePeerPreferred, state.RoutingModePeerOnly:
		return management.WorkerRequest{Mode: state.RoutingMode(mode)}, nil
	case state.RoutingModePinned:
		return management.WorkerRequest{}, fmt.Errorf(
			"waired worker set: --mode=pinned requires --pin=<peer>")
	default:
		return management.WorkerRequest{}, fmt.Errorf(
			"waired worker set: unknown --mode %q (want auto|local-only|peer-preferred|peer-only)", mode)
	}
}

// resolvePeerToDeviceID accepts either a DeviceID (returned verbatim
// when an exact match is found in the snapshot) or a peer name, and
// returns the canonical DeviceID. Names are matched case-sensitively;
// ambiguous names (two peers with the same name) are rejected so the
// operator must use the DeviceID.
//
// The value returned is the REAL DeviceID either way — it is the pin the
// daemon stores and the router matches candidates on, not something an
// operator reads. The candidates named in the ambiguity error are the
// other thing: that string is a CLI surface, so a public machine among
// them is named by its grant pseudonym (#739, public share spec §8.5).
// `waired ping` settled the same split first (#723).
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
		// No "DeviceIDs:" label any more: the list mixes device ids with
		// pseudonyms, so naming it after one of them would be wrong. This
		// is also the shape `waired ping`'s message already has.
		ids := make([]string, 0, len(nameMatches))
		for _, p := range nameMatches {
			id := peerDisplayID(p)
			if id == "" {
				id = inferencemesh.PublicPeerLabel
			}
			ids = append(ids, id)
		}
		return "", fmt.Errorf(
			"waired worker set: peer name %q is ambiguous — %d peers share it (%s). Use the DeviceID instead",
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
		// Which model, then whether it works — the order someone reads
		// when deciding whether this is still the node they wanted
		// (waired#1064). The label column is hand-padded to 13.
		fmt.Fprintf(out, "model:       %s\n", displayPinModel(resp.PinnedPeerModel))
		fmt.Fprintf(out, "status:      %s\n", displayPinStatus(resp.PinnedPeerStatus, resp.PinnedPeerCondition))
	}
	_, _ = w.Write(out.Bytes())
}

func displayMode(m state.RoutingMode) string {
	if m == "" {
		return string(state.RoutingModeAuto)
	}
	return string(m)
}

// displayPin names the pinned peer for the `worker:` line.
//
// The identifier shown is the daemon's PinnedPeerDisplayID — the grant
// pseudonym when the pin is a public machine — never PinnedPeerDeviceID,
// which is the routing key and may be a stranger's real device id
// (#739).
//
// The fallback is for an agent that predates the field: it reports no
// display identifier for any pin, and dropping the id entirely would
// blank the line for every own-network pin on an agent that has not been
// upgraded yet. A public-machine pin carried over from such an agent,
// whose peer is also missing from the snapshot, is the one case this
// still names by device id — recorded rather than closed, because
// closing it costs every pre-upgrade reader their identifier.
func displayPin(resp management.WorkerResponse) string {
	id := resp.PinnedPeerDisplayID
	if id == "" {
		id = resp.PinnedPeerDeviceID
	}
	if resp.PinnedPeerName != "" {
		return fmt.Sprintf("%s (%s)", resp.PinnedPeerName, id)
	}
	return id
}

// displayPinModel names the model the pinned peer is committed to.
// "unknown" rather than blank when the peer named none: an agent that
// predates waired#1064, or a peer that has dropped out of the snapshot,
// and both are "we cannot tell" rather than "it runs nothing".
func displayPinModel(model string) string {
	if model == "" {
		return "unknown"
	}
	return model
}

// displayPinStatus keeps the three statuses a caller branches on and
// uses the condition, when the peer gave one, for the parenthetical.
// "unavailable (peer present but not serving)" only ever restated the
// status; "unavailable (downloading its model)" is the thing an
// operator can act on (waired#1064).
func displayPinStatus(s, condition string) string {
	switch s {
	case "ok":
		return "ok (peer reachable, serving)"
	case "unavailable":
		if why := pinConditionDetail(condition); why != "" {
			return "unavailable (" + why + ")"
		}
		return "unavailable (peer present but not serving)"
	case "absent":
		return "absent (peer not in current mesh snapshot)"
	case "":
		return "unknown"
	default:
		return s
	}
}

// pinConditionDetail spells out the peer-reported conditions as the
// phrase that fits inside "unavailable (...)". Returns "" for the
// conditions that carry no reason, leaving the original wording.
func pinConditionDetail(c string) string {
	switch c {
	case signer.SubsystemStateLoading:
		return "downloading or loading its model"
	case signer.SubsystemStatePullFailed:
		return "its model download failed"
	case signer.SubsystemStateEngineFailed:
		return "its engine is down"
	case signer.SubsystemStateStarting:
		return "its engine is starting"
	case signer.SubsystemStateStopped:
		return "its engine was stopped to free memory"
	case signer.SubsystemStateAwaitingModel:
		return "no model chosen on that peer"
	case signer.SubsystemStateNoEngine:
		return "no engine on that peer"
	case signer.SubsystemStateDisabled:
		return "inference paused on that peer"
	}
	return ""
}
