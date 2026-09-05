package tray

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
)

// status_report.go builds the text behind the "Status…" row and behind
// every status row in the menu.
//
// Why a dialog and not a page: the rich reads this needs — /identity,
// /inference/mesh, /observability/state, /integration/claude — are
// socket-only. internal/management/socket.go's tcpReadRoutes is explicit
// that its five entries "are not a judgement about which reads are
// harmless", and platform/paths says the socket was chosen because
// "browsers and network peers cannot open a unix socket / named pipe,
// which is the point (waired#838)". Serving this as HTML would widen
// exactly the surface those two decisions narrowed. The tray already
// holds every fact, so it renders them itself.
//
// Two strings come out of one pass:
//
//   - dialog — what the message box shows. Windows' MessageBoxW and
//     macOS' `display dialog` render in the system's proportional font
//     and neither scrolls, so this is a short label/value list, never a
//     column-aligned table, and it is capped.
//   - details — what "Copy details" puts on the clipboard. No cap, every
//     peer, plus the diagnostic fields a support thread asks for next.
//
// Every peer word comes from internal/inferencemesh, the same predicates
// the menu rows and `waired peers list` read, so a machine reads the same
// in all three (waired#1064). PeerDisplayName in particular is what keeps
// a public-share peer under its pseudonym here (§8.5).

// statusPageLabel is the row that opens this report. The ellipsis is the
// product's own convention for a row that raises a window (the same one
// "Open Admin Console…" and "Sign out…" use).
const statusPageLabel = "Status…"

// statusCopiedToast is what confirms the clipboard write. The report can
// be long, and on a machine with no dialog backend it is the only sign
// anything happened at all.
const statusCopiedToast = "The Waired status report is on your clipboard."

// statusDialogMaxPeers caps the peer list in the dialog. Ten lines plus
// the four header lines and the local block stays inside a message box
// on a laptop screen; the rest is one "+N more" line pointing at the
// clipboard, which has no cap.
const statusDialogMaxPeers = 10

// statusReport renders the status summary twice: once for the dialog,
// once for the clipboard.
//
// It reads the MenuModel for everything the menu already says, so the
// dialog cannot contradict the row that opened it — a status line is
// quoted verbatim, not re-derived — and reads the Snapshot only for the
// facts no row has room for.
func statusReport(m MenuModel, snap Snapshot, version, buildSHA string, now time.Time) (dialog, details string) {
	if now.IsZero() {
		now = time.Now()
	}
	return renderStatus(m, snap, version, buildSHA, now, false),
		renderStatus(m, snap, version, buildSHA, now, true)
}

func renderStatus(m MenuModel, snap Snapshot, version, buildSHA string, now time.Time, full bool) string {
	var b strings.Builder

	// --- identity -------------------------------------------------
	if version != "" {
		line := "Waired " + version
		if buildSHA != "" {
			line += " (" + buildSHA + ")"
		}
		writeLine(&b, line)
	}
	if id := joinNonEmpty(" · ", m.DeviceName, m.OverlayIP, m.NetworkName); id != "" {
		writeLine(&b, id)
	}
	head := m.HeaderTitle
	if head == "" {
		head = "(state unknown)"
	}
	writeLine(&b, head+" · read at "+now.Format("15:04:05"))
	if m.StatusMsg != "" {
		writeLine(&b, m.StatusMsg)
	}
	if full {
		writeLine(&b, "host: "+runtime.GOOS+"/"+runtime.GOARCH)
		if m.AccountEmail != "" {
			writeLine(&b, "account: "+m.AccountEmail)
		}
		if m.ShowUpdate && m.UpdateVersion != "" {
			writeLine(&b, "update available: "+m.UpdateVersion)
		}
	}

	// --- this computer --------------------------------------------
	local := []string{
		m.StatusEngineLabel,
		// The engine's own reason, quoted from the row that carries it.
		// "⚠ Engine: engine failed" tells a person that Inference has the
		// reason, and this window is what that row opens — but the local
		// block listed everything EXCEPT the reason, while the peer block
		// forty lines down prints every other computer's engine error
		// (waired-agent#1136).
		m.EngineWarningLabel,
		m.ActiveModelLabel,
		m.ShareStateLabel,
		m.WorkerActiveLabel,
		m.MeshReachableLabel,
		m.StatusClaudeLabel,
	}
	if full {
		// The row above is one clamped line, because it is also a menu
		// label. A support thread wants the rest — the engine.log tail
		// folded into last_error is usually the whole diagnosis — and
		// "details" is documented above as having no cap. Read off the
		// Snapshot rather than the MenuModel because it is a fact no row
		// has room for, which is what this function reads the Snapshot
		// for.
		local = append(local, engineReasonDetail(snap, m.EngineWarningLabel))
		local = append(local, m.ClaudeHeader, m.ClaudeProxyLabel,
			m.OpenCodeHeader, m.OpenCodeConfigLabel,
			m.OpenClawHeader, m.OpenClawConfigLabel)
	}
	writeSection(&b, "THIS COMPUTER", local)

	// --- the other computers --------------------------------------
	// Skipped when there is nothing to report from — the daemon is not
	// answering, or has not answered yet. The tray learns about peers from
	// it, so "none yet" from a tray that has not looked would be a claim
	// about the mesh made by the one component that cannot see it.
	if snap.Health != HealthOffline && (snap.Mesh != nil || snap.Status != nil) {
		writeBlank(&b)
		writeLine(&b, "OTHER COMPUTERS"+peersHeadline(m, snap))
		for _, line := range peerLines(snap, full) {
			writeLine(&b, "  "+line)
		}
	}

	// --- notices --------------------------------------------------
	// The report is where a notice row goes when its own action cannot
	// be carried out, so what it was saying has to be readable here
	// (waired-agent#1205). The label carries the marker already.
	if len(m.Notices) > 0 {
		rows := make([]string, 0, len(m.Notices))
		for _, n := range m.Notices {
			rows = append(rows, n.Label)
		}
		writeSection(&b, "NOTICES", rows)
	}

	// --- recent activity ------------------------------------------
	if len(m.RecentActivityEntries) > 0 {
		rows := make([]string, 0, len(m.RecentActivityEntries))
		for _, r := range m.RecentActivityEntries {
			rows = append(rows, r.Label)
		}
		writeSection(&b, "RECENT", rows)
	}

	// --- map diagnostics (clipboard only) -------------------------
	if full && snap.Mesh != nil {
		mesh := snap.Mesh
		diag := []string{
			fmt.Sprintf("peer engines reachable: %t", mesh.Reachable),
			"snapshot generated: " + orDash(mesh.GeneratedAt),
			"newest map frame: " + orDash(mesh.MapReceivedAt),
			fmt.Sprintf("map age: %d ms (frame staleness limit %d ms)", mesh.MapAgeMS, mesh.FrameStalenessMS),
			fmt.Sprintf("advertised liveness limit: %d ms", mesh.StalenessThresholdMS),
		}
		writeSection(&b, "MESH MAP", diag)
	}

	if !full {
		writeBlank(&b)
		writeLine(&b, statusCopyHint)
	}
	return strings.TrimRight(b.String(), "\n")
}

// statusCopyHint is the dialog's last line. The button that answers it
// is labelled per OS by ShowStatus.
const statusCopyHint = "Copy details puts the full report, with every computer, on your clipboard."

// peersHeadline is the " — 2 of 4 serving" tail on the OTHER COMPUTERS
// heading. It reuses the top-level Peers row so the count cannot drift
// from the one the menu shows; a host with no peers has no such row, and
// says so in words instead.
func peersHeadline(m MenuModel, snap Snapshot) string {
	if m.StatusPeersLabel != "" {
		if _, tail, ok := strings.Cut(m.StatusPeersLabel, "Peers: "); ok {
			return " — " + tail
		}
		return " — " + m.StatusPeersLabel
	}
	if snap.Mesh != nil && len(snap.Mesh.Peers) > 0 {
		return fmt.Sprintf(" — %d known", len(snap.Mesh.Peers))
	}
	return " — none yet"
}

// peerLines renders one line per peer, newest vocabulary first: the same
// "<glyph> <name> — <model|reason>" the This device rows show, with the
// hardware the row had to drop to fit.
//
// The dialog form stops at statusDialogMaxPeers and says how many it
// left out; the clipboard form prints every peer and appends the
// diagnostic tail (identifier, address, transport, last error).
func peerLines(snap Snapshot, full bool) []string {
	hw := hardwareByDevice(snap.Status)
	path := pathByDevice(snap.Status)

	if snap.Mesh == nil || len(snap.Mesh.Peers) == 0 {
		return peerLinesFromStatus(snap.Status, full)
	}
	peers := append([]inferencemesh.PeerView(nil), snap.Mesh.Peers...)
	sort.SliceStable(peers, func(i, j int) bool {
		return peerSortKey(peers[i]) < peerSortKey(peers[j])
	})

	out := make([]string, 0, len(peers)+1)
	for i, p := range peers {
		if !full && i >= statusDialogMaxPeers {
			out = append(out, fmt.Sprintf("+%d more — on the clipboard", len(peers)-i))
			break
		}
		line := formatPeerRowLabel(p)
		if tail := formatHardwareTail(hw[p.DeviceID]); tail != "" && tail != hardwareUnknown {
			line += " · " + tail
		}
		if full {
			line += peerDiagnosticTail(p, path[p.DeviceID])
		}
		out = append(out, line)
	}
	return out
}

// peerLinesFromStatus is the pre-mesh rendering, for a daemon that
// exposes /status but not /inference/mesh. It says nothing about serving
// state because that daemon reports none — the same reason the peer rows
// fall back to hardware there (applyPeerHardware).
func peerLinesFromStatus(st *management.Status, full bool) []string {
	if st == nil || len(st.Peers) == 0 {
		return []string{"(none)"}
	}
	out := make([]string, 0, len(st.Peers)+1)
	for i, p := range st.Peers {
		if !full && i >= statusDialogMaxPeers {
			out = append(out, fmt.Sprintf("+%d more — on the clipboard", len(st.Peers)-i))
			break
		}
		out = append(out, formatPeerHardwareLabel(p))
	}
	return out
}

// peerDiagnosticTail is the clipboard-only detail: which identifier the
// viewer may use for this peer, its overlay address, how packets reach
// it, and the last thing its engine complained about.
//
// PeerDisplayID, never DeviceID — a grant peer's real identifier may not
// be shown even in a support paste (§8.5); a grant with no pseudonym has
// nothing showable and gets none.
func peerDiagnosticTail(p inferencemesh.PeerView, transport string) string {
	parts := make([]string, 0, 5)
	if id, ok := inferencemesh.PeerDisplayID(p); ok && id != "" {
		parts = append(parts, "id "+id)
	}
	if p.Grant == nil && p.OverlayIP != "" {
		parts = append(parts, p.OverlayIP)
	}
	if transport != "" {
		parts = append(parts, transport)
	}
	if p.Silent {
		parts = append(parts, "silent")
	}
	if p.Stale {
		parts = append(parts, "stale")
	}
	if p.InferenceState != nil {
		if p.InferenceState.Type != "" {
			parts = append(parts, p.InferenceState.Type)
		}
		if p.InferenceState.LastCheck != "" {
			parts = append(parts, "last check "+p.InferenceState.LastCheck)
		}
		if p.InferenceState.LastError != "" {
			parts = append(parts, "error: "+p.InferenceState.LastError)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n      " + strings.Join(parts, " · ")
}

// peerSortKey orders peers by the name they are displayed under, so the
// report reads in the same order as the menu and `waired peers list`.
func peerSortKey(p inferencemesh.PeerView) string {
	if name, ok := inferencemesh.PeerDisplayName(p); ok {
		return strings.ToLower(name)
	}
	return "￿"
}

// hardwareByDevice indexes the /status peer hardware by device id so a
// mesh row can carry it. The key is never displayed, so using DeviceID
// for a grant peer here does not put it on a surface.
func hardwareByDevice(st *management.Status) map[string]*management.PeerHardware {
	out := map[string]*management.PeerHardware{}
	if st == nil {
		return out
	}
	for _, p := range st.Peers {
		if p.Hardware != nil {
			out[p.DeviceID] = p.Hardware
		}
	}
	return out
}

// pathByDevice indexes how each peer is currently reached ("direct" /
// "relay") with the RTT the agent last measured on that path.
func pathByDevice(st *management.Status) map[string]string {
	out := map[string]string{}
	if st == nil {
		return out
	}
	for _, p := range st.Peers {
		switch p.CurrentPath {
		case "direct":
			out[p.DeviceID] = fmt.Sprintf("direct %.0f ms", p.DirectRTTMS)
		case "relay":
			out[p.DeviceID] = fmt.Sprintf("relay %.0f ms", p.RelayRTTMS)
		case "":
		default:
			out[p.DeviceID] = p.CurrentPath
		}
	}
	return out
}

func writeSection(b *strings.Builder, title string, rows []string) {
	kept := make([]string, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r) != "" {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		return
	}
	writeBlank(b)
	writeLine(b, title)
	for _, r := range kept {
		writeLine(b, "  "+r)
	}
}

func writeLine(b *strings.Builder, s string) {
	b.WriteString(s)
	b.WriteString("\n")
}

func writeBlank(b *strings.Builder) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
}

func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// engineReasonDetail is the serving engine's failure reason in full, for the
// clipboard. Empty when there is no reason, or when the one-line row in the
// dialog already carries all of it — repeating a short reason under a
// second label reads as two different facts.
func engineReasonDetail(snap Snapshot, shown string) string {
	r, ok := servingRuntime(snap.Inference)
	if !ok || strings.TrimSpace(r.LastError) == "" {
		return ""
	}
	full := strings.TrimSpace(r.LastError)
	if full == strings.TrimSpace(strings.TrimPrefix(shown, glyphFault)) {
		return ""
	}
	return "engine reason (full): " + full
}
