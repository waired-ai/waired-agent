package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestPickerRepublishWanted is the whole scheduling rule. A pure function so
// every arm is a row rather than a process (CLAUDE.md §Test discipline).
func TestPickerRepublishWanted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fromManaged bool
		out         pickerWriteOutcome
		want        bool
	}{
		{
			// The case the fix exists for: the rows moved, and a session
			// that has already read the old ones is running.
			name: "the hook changed the rows", fromManaged: true,
			out:  pickerWriteOutcome{LineupChanged: true},
			want: true,
		},
		{
			// The retired cache is read once at startup. Taking it away
			// mid-session reaches nobody, so it is not worth a second write.
			name: "the hook only took the retired cache away", fromManaged: true,
			out:  pickerWriteOutcome{CacheRemoved: true},
			want: false,
		},
		{
			// The common launch. Nothing moved, so the running session
			// already holds what is on disk.
			name: "the hook changed nothing", fromManaged: true,
			out:  pickerWriteOutcome{},
			want: false,
		},
		{
			// `waired claude enable`, elevated, from a shell. No session is
			// watching the file and this process may be root, so a child
			// left behind would have nothing to tell and the wrong identity
			// to tell it with.
			name: "the elevated enable path wrote the rows", fromManaged: false,
			out:  pickerWriteOutcome{LineupChanged: true},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickerRepublishWanted(tc.fromManaged, tc.out); got != tc.want {
				t.Errorf("pickerRepublishWanted(%v, %+v) = %v, want %v",
					tc.fromManaged, tc.out, got, tc.want)
			}
		})
	}
}

// TestPickerRepublishSleepsDerivation: the offsets are measured from the
// child's start, and the loop waits between them. Getting this wrong would
// put the second write 20 s after the first rather than 20 s into the
// session, which is a different measurement than the one recorded.
func TestPickerRepublishSleepsDerivation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offsets []time.Duration
		want    []time.Duration
	}{
		{"the shipped schedule", pickerRepublishOffsets, []time.Duration{8 * time.Second, 12 * time.Second}},
		{"one attempt", []time.Duration{8 * time.Second}, []time.Duration{8 * time.Second}},
		{"none", nil, []time.Duration{}},
		{"out of order waits no time", []time.Duration{20 * time.Second, 8 * time.Second},
			[]time.Duration{20 * time.Second, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickerRepublishSleeps(tc.offsets)
			if len(got) != len(tc.want) {
				t.Fatalf("pickerRepublishSleeps(%v) = %v, want %v", tc.offsets, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("sleep %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPickerRepublishWritesOnEveryScheduledAttempt: the child waits the
// measured schedule and writes each time, not only at the end.
//
// The injected sleep does two things a real one cannot: it records what it
// was asked to wait, and it puts the file back into a non-canonical spelling
// first. So the file's shape at the START of the second wait is what proves
// the first attempt wrote, and its shape at the end proves the second did.
// Dropping either attempt fails. Nothing here waits for real time.
func TestPickerRepublishWritesOnEveryScheduledAttempt(t *testing.T) {
	home := t.TempDir()
	path := claudecode.SettingsPath(home)
	rows := []claudecode.PickerRow{
		{Model: "waired/peer-attic", Label: "Waired peer: attic", Description: "qwen3.5-9b"},
	}
	if _, err := claudecode.WritePickerLineup(path, rows); err != nil {
		t.Fatal(err)
	}
	canonical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The same lineup, written the way nothing in waired writes it.
	scramble := []byte(`{"modelPicker":{"options":[{"description":"qwen3.5-9b",` +
		`"label":"Waired peer: attic","model":"waired/peer-attic"}]}}`)

	var slept []time.Duration
	var sawCanonicalAtStartOfWait []bool
	sleep := func(d time.Duration) {
		on, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the settings file: %v", err)
		}
		sawCanonicalAtStartOfWait = append(sawCanonicalAtStartOfWait, bytes.Equal(on, canonical))
		slept = append(slept, d)
		if err := os.WriteFile(path, scramble, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := runPickerRepublish(home, pickerRepublishOffsets, sleep); err != nil {
		t.Fatalf("runPickerRepublish: %v", err)
	}

	want := pickerRepublishSleeps(pickerRepublishOffsets)
	if len(slept) != len(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, slept[i], want[i])
		}
	}
	if len(sawCanonicalAtStartOfWait) < 2 || sawCanonicalAtStartOfWait[1] != true {
		t.Errorf("the first scheduled attempt did not write: canonical-at-start-of-wait = %v",
			sawCanonicalAtStartOfWait)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, canonical) {
		t.Errorf("the last scheduled attempt did not write\n got: %s\nwant: %s", got, canonical)
	}
}

// TestPickerRepublishDoesNotBringBackRowsLogoutRemoved.
//
// PIN: product contract — a signed-out computer offers no Waired rows in
// /model (waired-agent#1310). The republish fires seconds after the write it
// follows, so `waired logout` landing in between is the case that decides
// whether it may carry remembered rows. It may not.
func TestPickerRepublishDoesNotBringBackRowsLogoutRemoved(t *testing.T) {
	home := t.TempDir()
	path := claudecode.SettingsPath(home)
	if _, err := claudecode.WritePickerLineup(path, []claudecode.PickerRow{{Model: "waired"}}); err != nil {
		t.Fatal(err)
	}
	if removed, err := claudecode.RemovePickerLineup(path); err != nil || !removed {
		t.Fatalf("RemovePickerLineup = (%v, %v)", removed, err)
	}

	if err := runPickerRepublish(home, pickerRepublishOffsets, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}

	if kind, rows := claudecode.DetectPickerLineup(path); kind != claudecode.PickerLineupNone {
		t.Errorf("the rows came back after a sign-out: kind=%v rows=%v", kind, rows)
	}
}

// TestPickerRepublishStaysSilent: Claude Code reads a SessionStart hook's
// stdout as the user's session context. The child has no terminal today, but
// this code is one refactor away from running in the hook's own process, so
// the silence is pinned where it is decided rather than where it happens to
// be harmless.
func TestPickerRepublishStaysSilent(t *testing.T) {
	seed := func(t *testing.T, body string) string {
		t.Helper()
		home := t.TempDir()
		path := claudecode.SettingsPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return home
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"rows it owns", `{"modelPicker":{"options":[{"model":"waired"}]}}`},
		{"someone else's lineup", `{"modelPicker":{"options":[{"model":"us.anthropic.claude-opus-4-8"}]}}`},
		{"a file it cannot read", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := seed(t, tc.body)
			out, errOut := captureBothStreams(t, func() {
				if err := runPickerRepublish(home, pickerRepublishOffsets, func(time.Duration) {}); err != nil {
					t.Errorf("runPickerRepublish: %v", err)
				}
			})
			if out != "" {
				t.Errorf("wrote to stdout, which Claude Code pastes into the session: %q", out)
			}
			if errOut != "" {
				t.Errorf("wrote to stderr: %q", errOut)
			}
		})
	}
}

// captureBothStreams is captureStdout for both streams at once.
func captureBothStreams(t *testing.T, fn func()) (stdoutText, stderrText string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	done := make(chan [2]string, 1)
	go func() {
		var o, e bytes.Buffer
		_, _ = io.Copy(&o, outR)
		_, _ = io.Copy(&e, errR)
		done <- [2]string{o.String(), e.String()}
	}()
	fn()
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	both := <-done
	return both[0], both[1]
}
