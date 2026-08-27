package tray

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The status report is what every row that names a state opens, and what
// "Status…" opens. These pin the two properties the medium imposes — the
// dialog is short and the clipboard is complete — and the one the product
// imposes: a public-share peer is named by its pseudonym here too.
//
// Owner request, 2026-08-28: the rows had to stop being greyed out, and a
// row that is no longer grey has to do something when clicked.

func testReportNow() time.Time {
	return time.Date(2026, 8, 28, 14, 32, 5, 0, time.UTC)
}

// connectedModel is a MenuModel as Update would leave it on a healthy
// engine-less host served by a peer — the shape waired-agent#1032 was
// found on.
func connectedModel() MenuModel {
	return MenuModel{
		Kind:              MenuConnected,
		HeaderTitle:       "● Connected",
		AccountEmail:      "someone@example.com",
		DeviceName:        "pc-dell-premium",
		OverlayIP:         "100.64.0.3",
		NetworkName:       "example-net",
		StatusEngineLabel: "○ Engine: off on this computer",
		StatusPeersLabel:  "● Peers: 2 of 3 serving",
		StatusClaudeLabel: "● Claude Code: routed through Waired",
		WorkerActiveLabel: "Worker: sv-evox2 (pinned)",
	}
}

func meshWith(peers ...inferencemesh.PeerView) *inferencemesh.Snapshot {
	return &inferencemesh.Snapshot{
		GeneratedAt:          "2026-08-28T14:32:00Z",
		Reachable:            true,
		StalenessThresholdMS: 90000,
		FrameStalenessMS:     120000,
		MapReceivedAt:        "2026-08-28T14:31:58Z",
		MapAgeMS:             7000,
		Peers:                peers,
	}
}

func servingPeer(id, name, model string) inferencemesh.PeerView {
	return inferencemesh.PeerView{
		DeviceID: id, DeviceName: name,
		InferenceState: &signer.InferenceState{
			Reachable: true, Models: []string{model + ":tag"}, ActiveModel: model,
			Type: "ollama", LastCheck: "2026-08-28T14:31:55Z",
		},
	}
}

// TestStatusReport_QuotesTheRowThatOpenedIt is the point of the whole
// report: the line the user clicked appears in it verbatim. Re-deriving
// the state here is how the tray came to contradict itself before
// (waired-agent#1032).
func TestStatusReport_QuotesTheRowThatOpenedIt(t *testing.T) {
	m := connectedModel()
	snap := Snapshot{Health: HealthOnline, Mesh: meshWith(servingPeer("dev_b", "sv-evox2", "qwen3.6-35b-a3b"))}

	dialog, details := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	for _, want := range []string{
		"Waired 0.0.3-rc4 (90dd4a5)",
		"pc-dell-premium · 100.64.0.3 · example-net",
		"● Connected · read at 14:32:05",
		"○ Engine: off on this computer",
		"● Claude Code: routed through Waired",
		"Worker: sv-evox2 (pinned)",
		"OTHER COMPUTERS — 2 of 3 serving",
		"● sv-evox2 — qwen3.6-35b-a3b",
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("dialog is missing %q\n---\n%s", want, dialog)
		}
		if !strings.Contains(details, want) {
			t.Errorf("details is missing %q\n---\n%s", want, details)
		}
	}
}

// TestStatusReport_DialogStaysShort is a contract, not a preference:
// Windows' MessageBoxW and macOS' `display dialog` do not scroll, so a
// report that outgrows the box is a report nobody can read the end of.
func TestStatusReport_DialogStaysShort(t *testing.T) {
	peers := make([]inferencemesh.PeerView, 0, MaxPeerRows)
	for i := range MaxPeerRows {
		peers = append(peers, servingPeer(
			string(rune('a'+i)), "computer-"+string(rune('a'+i)), "qwen3.8-27b"))
	}
	m := connectedModel()
	m.RecentActivityEntries = make([]RecentActivityRow, 0, MaxRecentActivity)
	for range MaxRecentActivity {
		m.RecentActivityEntries = append(m.RecentActivityEntries,
			RecentActivityRow{Label: "14:02  peer unavailable → Anthropic (qwen3.8-27b)"})
	}
	snap := Snapshot{Health: HealthOnline, Mesh: meshWith(peers...)}

	dialog, details := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	if lines := strings.Count(dialog, "\n") + 1; lines > 40 {
		t.Errorf("dialog is %d lines, want ≤ 40 — a message box does not scroll\n---\n%s", lines, dialog)
	}
	if n := len(dialog); n > 2000 {
		t.Errorf("dialog is %d bytes, want ≤ 2000", n)
	}
	// The cap must announce itself: a list silently ending at ten reads
	// as a fleet of ten.
	if !strings.Contains(dialog, "+6 more — on the clipboard") {
		t.Errorf("dialog does not say what it left out\n---\n%s", dialog)
	}
	// …and the clipboard must actually have them.
	for i := range MaxPeerRows {
		name := "computer-" + string(rune('a'+i))
		if !strings.Contains(details, name) {
			t.Errorf("details is missing peer %q — the dialog promised it was there", name)
		}
	}
}

// TestStatusReport_PublicSharePeerKeepsItsPseudonym: public share spec
// §8.5 says a stranger's device identifier may not reach a surface, and
// a support paste is a surface. The report reads
// inferencemesh.PeerDisplayName/PeerDisplayID for exactly that reason.
func TestStatusReport_PublicSharePeerKeepsItsPseudonym(t *testing.T) {
	peer := servingPeer("dev_real_identifier", "someones-laptop", "qwen3.8-27b")
	peer.OverlayIP = "100.64.0.9"
	peer.Grant = &signer.PeerGrant{ID: "grant-1", Pseudonym: "guest-7"}
	m := connectedModel()
	snap := Snapshot{Health: HealthOnline, Mesh: meshWith(peer)}

	dialog, details := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	for name, got := range map[string]string{"dialog": dialog, "details": details} {
		if !strings.Contains(got, "guest-7") {
			t.Errorf("%s does not name the peer by its pseudonym\n---\n%s", name, got)
		}
		for _, leak := range []string{"dev_real_identifier", "someones-laptop", "100.64.0.9"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s leaks %q for a public-share peer (spec §8.5)\n---\n%s", name, leak, got)
			}
		}
	}
}

// TestStatusReport_NoPeersSaysSo — a host on its own gets no Peers row in
// the menu, so the report has to say in words what the missing row would
// have said. "0 of 0 serving" is a fact about nothing (peersStatusRow).
func TestStatusReport_NoPeersSaysSo(t *testing.T) {
	m := connectedModel()
	m.StatusPeersLabel = ""
	snap := Snapshot{Health: HealthOnline, Mesh: meshWith()}

	dialog, _ := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	if !strings.Contains(dialog, "OTHER COMPUTERS — none yet") {
		t.Errorf("dialog does not say the mesh is empty\n---\n%s", dialog)
	}
	if !strings.Contains(dialog, "(none)") {
		t.Errorf("dialog leaves the peer list blank instead of saying it is empty\n---\n%s", dialog)
	}
}

// TestStatusReport_FallsBackToStatusPeers keeps a daemon that predates
// /inference/mesh readable: it reports peers with hardware and no serving
// state, which is the same fallback the menu rows take
// (applyPeerHardware).
func TestStatusReport_FallsBackToStatusPeers(t *testing.T) {
	m := connectedModel()
	m.StatusPeersLabel = ""
	snap := Snapshot{
		Health: HealthOnline,
		Status: &management.Status{Peers: []management.PeerStatus{
			{DeviceID: "dev_b", DeviceName: "sv-evox2", Hardware: &management.PeerHardware{
				GPUModel: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24576,
			}},
		}},
	}

	dialog, _ := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	if !strings.Contains(dialog, "sv-evox2 — RTX 4090 (24 GB)") {
		t.Errorf("dialog does not fall back to the hardware rendering\n---\n%s", dialog)
	}
}

// TestStatusReport_DaemonDownSaysSo. The report is built from the last
// poll, and when that poll failed the tray has nothing current to show —
// so it must show the daemon-down state rather than the last good one.
func TestStatusReport_DaemonDownSaysSo(t *testing.T) {
	m := MenuModel{
		Kind:        MenuDaemonDown,
		HeaderTitle: "⚠ Waired agent is not running",
		StatusMsg:   "Start the Waired background service to continue.",
	}
	snap := Snapshot{Health: HealthOffline}

	dialog, _ := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	if !strings.Contains(dialog, "⚠ Waired agent is not running") {
		t.Errorf("dialog does not carry the daemon-down header\n---\n%s", dialog)
	}
	if !strings.Contains(dialog, "Start the Waired background service") {
		t.Errorf("dialog drops the hint the menu row was showing\n---\n%s", dialog)
	}
}

// TestStatusReport_DetailsCarryWhatTheDialogCannot — the clipboard is the
// half with no length limit, so the diagnostics a support thread asks for
// next live there and nowhere else.
func TestStatusReport_DetailsCarryWhatTheDialogCannot(t *testing.T) {
	peer := servingPeer("dev_b", "sv-evox2", "qwen3.6-35b-a3b")
	peer.OverlayIP = "100.64.0.4"
	peer.Silent = true
	m := connectedModel()
	snap := Snapshot{
		Health: HealthOnline,
		Mesh:   meshWith(peer),
		Status: &management.Status{Peers: []management.PeerStatus{
			{DeviceID: "dev_b", CurrentPath: "relay", RelayRTTMS: 28},
		}},
	}

	dialog, details := statusReport(m, snap, "0.0.3-rc4", "90dd4a5", testReportNow())

	for _, want := range []string{
		"id dev_b", "100.64.0.4", "relay 28 ms", "silent", "ollama",
		"MESH MAP", "map age: 7000 ms", "host: ",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details is missing %q\n---\n%s", want, details)
		}
		if strings.Contains(dialog, want) {
			t.Errorf("dialog carries %q, which belongs in the clipboard half only\n---\n%s", want, dialog)
		}
	}
	// The dialog says where the rest is.
	if !strings.Contains(dialog, statusCopyHint) {
		t.Errorf("dialog does not point at the clipboard\n---\n%s", dialog)
	}
	if strings.Contains(details, statusCopyHint) {
		t.Errorf("details carries the dialog's own footer")
	}
}

// TestStatusReport_SectionsWithNothingToSayAreDropped — an empty heading
// is a row that promises detail and delivers none.
func TestStatusReport_SectionsWithNothingToSayAreDropped(t *testing.T) {
	m := MenuModel{Kind: MenuConnected, HeaderTitle: "● Connected"}
	snap := Snapshot{Health: HealthOnline}

	dialog, _ := statusReport(m, snap, "", "", testReportNow())

	if strings.Contains(dialog, "THIS COMPUTER") {
		t.Errorf("dialog renders an empty THIS COMPUTER section\n---\n%s", dialog)
	}
	if strings.Contains(dialog, "RECENT") {
		t.Errorf("dialog renders an empty RECENT section\n---\n%s", dialog)
	}
	if strings.HasPrefix(dialog, "\n") {
		t.Errorf("dialog starts with a blank line\n---\n%q", dialog)
	}
}

// TestOnShowStatus_CopiesOnlyWhenAsked drives the handler across both
// arms of the dialog. The dialog is the seam; the clipboard is what the
// user gets. Answering "Close" must leave the clipboard alone — this row
// is a report, and a report that overwrites what you were pasting is a
// worse row than the greyed-out one it replaced.
func TestOnShowStatus_CopiesOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		copy     bool
		wantClip int
	}{
		{name: "Close leaves the clipboard alone", copy: false, wantClip: 0},
		{name: "Copy details writes the full report", copy: true, wantClip: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := resetSeams(t)
			l.mu.Lock()
			l.statusCopy = tc.copy
			l.mu.Unlock()

			tr := &tray{opts: Options{Version: "0.0.3-rc4", BuildSHA: "90dd4a5"}}
			tr.last = connectedModel()
			tr.lastSnap = Snapshot{Health: HealthOnline,
				Mesh: meshWith(servingPeer("dev_b", "sv-evox2", "qwen3.6-35b-a3b"))}

			tr.onShowStatus()

			shown := l.snapshot(&l.statuses)
			if len(shown) != 1 {
				t.Fatalf("dialog shown %d times, want 1", len(shown))
			}
			if !strings.Contains(shown[0], "● Connected") {
				t.Errorf("dialog body does not carry the header:\n%s", shown[0])
			}
			clip := l.snapshot(&l.clipboard)
			if len(clip) != tc.wantClip {
				t.Fatalf("clipboard writes = %d, want %d", len(clip), tc.wantClip)
			}
			if tc.wantClip == 1 && !strings.Contains(clip[0], "MESH MAP") {
				t.Errorf("clipboard got the dialog half, not the details half:\n%s", clip[0])
			}
		})
	}
}

// TestOnShowStatus_OneDialogAtATime: some two dozen rows open this, and
// the dialog blocks on the user. Without the in-flight guard a handful of
// clicks stacks a handful of message boxes on top of each other.
func TestOnShowStatus_OneDialogAtATime(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{opts: Options{Version: "0.0.3-rc4"}}
	tr.last = connectedModel()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	showStatus = func(body string) bool {
		seams.add(&seams.statuses, body)
		entered <- struct{}{}
		<-release
		return false
	}
	t.Cleanup(installSeamStubs)

	go tr.onShowStatus()
	<-entered // the first dialog is up and waiting on the user
	tr.onShowStatus()
	close(release)

	if shown := l.snapshot(&l.statuses); len(shown) != 1 {
		t.Errorf("dialogs shown = %d, want 1 — the second click stacked a box", len(shown))
	}
}
