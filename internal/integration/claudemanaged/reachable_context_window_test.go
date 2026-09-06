package claudemanaged

import "testing"

// A computer with no engine of its own declares a window it can reach
// (waired-agent#1246).
//
// PIN: product contract. Before this, LocalContextWindow was 0 on such a
// host, nothing was written, and Claude Code both assumed its own 200k
// default and showed "isn't described by this version's model catalog" on
// every Waired row — the notice this variable exists to suppress. Every row
// that host offers is a peer row, so a peer's window is not an approximation
// of something better; it is the only number available.

func TestDeclaredContextWindow_PrefersThisHostAndFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		local, peer int
		want        int
	}{
		{name: "an engine here answers for itself", local: 32768, peer: 8192, want: 32768},
		{name: "no engine here falls back to what it can reach", local: 0, peer: 8192, want: 8192},
		{name: "no engine and nothing reachable declares nothing", local: 0, peer: 0, want: 0},
		// The fallback never outranks a resolved local window, even a
		// smaller one: on a host that serves, its own number is exact for
		// the row most people use.
		{name: "a bigger peer does not override a serving host", local: 4096, peer: 131072, want: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (WriteOptions{LocalContextWindow: tc.local, PeerContextWindow: tc.peer}).
				DeclaredContextWindow(); got != tc.want {
				t.Errorf("WriteOptions.DeclaredContextWindow() = %d, want %d", got, tc.want)
			}
			// Remove has to recognise exactly what Write would have put there.
			if got := (RemoveOptions{LocalContextWindow: tc.local, PeerContextWindow: tc.peer}).
				DeclaredContextWindow(); got != tc.want {
				t.Errorf("RemoveOptions.DeclaredContextWindow() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWriteWithOptions_EnginelessHostDeclaresAReachableWindow(t *testing.T) {
	p := withTempPath(t)
	if _, err := WriteWithOptions(testBaseURL, WriteOptions{
		ModelRouteDirectives: true, LocalContextWindow: 0, PeerContextWindow: 131072,
	}); err != nil {
		t.Fatalf("WriteWithOptions: %v", err)
	}
	if got := envOf(t, p)[maxContextTokensKey]; got != "131072" {
		t.Errorf("%s = %v, want %q", maxContextTokensKey, got, "131072")
	}
}

// The value written from a peer's window must be recognisable as ours, or
// `waired claude disable` leaves it behind to steer every session that
// starts on that computer afterwards (the shape waired-agent#1174 warns
// about).
func TestRemoveWithOptions_ScrubsAWindowWrittenFromAPeer(t *testing.T) {
	p := withTempPath(t)
	// SOMETHING_ELSE keeps the file on disk after the scrub, so the
	// assertion is about the one key rather than about file removal.
	seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
		`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"131072","SOMETHING_ELSE":"1"`)
	if _, err := RemoveWithOptions(RemoveOptions{
		LocalContextWindow: 0, PeerContextWindow: 131072,
	}); err != nil {
		t.Fatalf("RemoveWithOptions: %v", err)
	}
	if got, ok := envOf(t, p)[maxContextTokensKey]; ok {
		t.Errorf("%s = %v, want it scrubbed", maxContextTokensKey, got)
	}
}

// And an operator's own value still survives, which is the half the
// recognition exists to protect.
func TestRemoveWithOptions_LeavesAnOperatorsWindowAlone(t *testing.T) {
	p := withTempPath(t)
	seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
		`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"777777"`)
	if _, err := RemoveWithOptions(RemoveOptions{
		LocalContextWindow: 0, PeerContextWindow: 131072,
	}); err != nil {
		t.Fatalf("RemoveWithOptions: %v", err)
	}
	if got := envOf(t, p)[maxContextTokensKey]; got != "777777" {
		t.Errorf("%s = %v, want the operator's own value untouched", maxContextTokensKey, got)
	}
}
