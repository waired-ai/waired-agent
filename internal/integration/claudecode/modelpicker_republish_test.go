package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func seedSettings(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestRepublishRewritesAnOwnedLineup: the republish has to actually write, not
// merely report that it did. The seed below says the same thing the writer
// would say, in a different spelling — different key order, different
// indentation — so a republish that returned true without touching the file
// would leave those bytes behind and fail here.
//
// Bytes rather than mtime: a settings watch reacts to the write, and the
// write is what this asserts. Nothing in this test sleeps.
func TestRepublishRewritesAnOwnedLineup(t *testing.T) {
	rows := []PickerRow{
		{Model: "waired", Label: "Waired", Description: "Any of your computers"},
		{Model: "waired/peer-attic", Label: "Waired peer: attic", Description: "qwen3.5-9b"},
	}
	seed := `{"modelPicker":{"options":[` +
		`{"label":"Waired","description":"Any of your computers","model":"waired"},` +
		`{"label":"Waired peer: attic","description":"qwen3.5-9b","model":"waired/peer-attic"}` +
		`]},"statusLine":{"type":"command","command":"keep me"}}`
	p := seedSettings(t, seed)

	rewritten, err := RepublishPickerLineup(p)
	if err != nil || !rewritten {
		t.Fatalf("RepublishPickerLineup = (%v, %v), want (true, nil)", rewritten, err)
	}

	// What the writer would have produced from the same rows, into a file
	// carrying the same neighbouring key.
	want := seedSettings(t, `{"statusLine":{"type":"command","command":"keep me"}}`)
	if _, err := WritePickerLineup(want, rows); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(wantBytes) {
		t.Errorf("republished file is not what the writer writes\n got: %s\nwant: %s", got, wantBytes)
	}
}

// TestRepublishLeavesEverythingElseAlone.
//
// PIN: product contract — `waired logout` and `waired claude disable` take the
// rows out of /model, and nothing puts them back (waired-agent#1310). The
// republish runs seconds after the write it follows, so a sign-out in between
// is exactly the case it must not undo. Reading the file each time is what
// makes that structural: there are no remembered rows to write.
//
// Foreign and unreadable are the same posture for the same reason the writer
// has it — waired supplies the lineup whole, so writing over someone else's
// deletes it.
func TestRepublishLeavesEverythingElseAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"the rows were removed", `{"statusLine":{"type":"command","command":"x"}}`},
		{"there is no file", ""},
		{"an empty lineup", `{"modelPicker":{"options":[]}}`},
		{"someone else's lineup", `{"modelPicker":{"options":[{"model":"us.anthropic.claude-opus-4-8"}]}}`},
		{"one foreign row among ours", `{"modelPicker":{"options":[{"model":"waired"},{"model":"opus"}]}}`},
		{"not JSON", `{`},
		{"the key is not a lineup", `{"modelPicker":"yes"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := seedSettings(t, tc.body)
			before, beforeErr := os.ReadFile(p)

			rewritten, err := RepublishPickerLineup(p)
			if err != nil {
				t.Fatalf("RepublishPickerLineup errored: %v", err)
			}
			if rewritten {
				t.Errorf("RepublishPickerLineup = true, want false")
			}

			after, afterErr := os.ReadFile(p)
			if (beforeErr == nil) != (afterErr == nil) {
				t.Fatalf("the file appeared or vanished: before %v, after %v", beforeErr, afterErr)
			}
			if beforeErr == nil && string(before) != string(after) {
				t.Errorf("the file was rewritten\n before: %s\n after: %s", before, after)
			}
		})
	}
}

// TestRepublishCarriesTheLatestRowsNotTheEarlierOnes: two `claude` launches
// starting together both compute rows, and the mesh may have moved between
// their reads. The second write wins, and the first launch's republish — which
// fires seconds later — must not put the first launch's rows back.
//
// This is the test that justifies the signature: a republish that took rows
// would have to take the caller's remembered ones, and could not pass.
func TestRepublishCarriesTheLatestRowsNotTheEarlierOnes(t *testing.T) {
	p := seedSettings(t, "")
	first := []PickerRow{{Model: "waired/peer-attic", Label: "Waired peer: attic", Description: "qwen3.5-9b"}}
	second := []PickerRow{{Model: "waired/peer-studio", Label: "Waired peer: studio", Description: "qwen3.6-35b-a3b"}}
	if _, err := WritePickerLineup(p, first); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePickerLineup(p, second); err != nil {
		t.Fatal(err)
	}

	if _, err := RepublishPickerLineup(p); err != nil {
		t.Fatal(err)
	}

	kind, got := DetectPickerLineup(p)
	if kind != PickerLineupOurs {
		t.Fatalf("kind = %v, want PickerLineupOurs", kind)
	}
	if len(got) != 1 || got[0].Model != second[0].Model {
		t.Errorf("republished rows = %v, want %v", got, second)
	}
}
