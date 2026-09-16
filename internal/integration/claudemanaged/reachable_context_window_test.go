package claudemanaged

import "testing"

// A computer with no engine of its own used to write the smallest window it
// could reach (waired-agent#1246). That value must still be recognisable as
// ours, or `waired claude disable` leaves it behind to steer every session
// that starts on that computer afterwards (the shape waired-agent#1174 warns
// about). Nothing writes it since waired-agent#1396.
func TestRemoveWithOptions_ScrubsAWindowWrittenFromAPeer(t *testing.T) {
	p := withTempPath(t)
	// SOMETHING_ELSE keeps the file on disk after the scrub, so the
	// assertion is about the one key rather than about file removal.
	seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
		`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"131072","SOMETHING_ELSE":"1"`)
	if _, err := RemoveWithOptions(RemoveOptions{PriorContextWindow: 131072}); err != nil {
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
	if _, err := RemoveWithOptions(RemoveOptions{PriorContextWindow: 131072}); err != nil {
		t.Fatalf("RemoveWithOptions: %v", err)
	}
	if got := envOf(t, p)[maxContextTokensKey]; got != "777777" {
		t.Errorf("%s = %v, want the operator's own value untouched", maxContextTokensKey, got)
	}
}
