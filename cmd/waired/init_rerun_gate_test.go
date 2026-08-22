package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestAskBeforeRerunningSetup pins which hosts get the question.
//
// Product contract (waired-ai/waired-agent#782 + the owner ruling on
// waired-ai/waired-agent#599, 2026-08-09): a re-run replays the whole
// install conversation, and what makes that safe is that choosing the same
// engine and the same model again changes nothing. The gate is where that
// clause is honoured — so it fires exactly for a host that is already set
// up and serving, and for nothing else.
func TestAskBeforeRerunningSetup(t *testing.T) {
	configured := rerunFacts{
		Interactive:     true,
		HasModelHistory: true,
		SubsystemState:  signer.SubsystemStateReady,
		ActiveModelID:   "qwen3.5-2b",
	}
	for _, tc := range []struct {
		name  string
		facts func(rerunFacts) rerunFacts
		want  bool
	}{
		{
			name:  "a configured host that is serving",
			facts: func(f rerunFacts) rerunFacts { return f },
			want:  true,
		},
		{
			// --non-interactive was already non-destructive at both speed
			// gates ("keep" on every arm), and a script that cannot answer
			// must not be stopped by a question.
			name:  "non-interactive",
			facts: func(f rerunFacts) rerunFacts { f.Interactive = false; return f },
			want:  false,
		},
		{
			// The flag IS the answer. Asking would be asking twice.
			name:  "the invocation already said what it wants",
			facts: func(f rerunFacts) rerunFacts { f.ExplicitIntent = true; return f },
			want:  false,
		},
		{
			name:  "a first install",
			facts: func(f rerunFacts) rerunFacts { f.HasModelHistory = false; return f },
			want:  false,
		},
		{
			// The #313 resume: a stuck setup is being finished, not
			// re-run. It is not serving, so it never reaches the question.
			name: "mid-setup, nothing serving yet",
			facts: func(f rerunFacts) rerunFacts {
				f.SubsystemState = "awaiting_model"
				f.ActiveModelID = ""
				return f
			},
			want: false,
		},
		{
			// Local inference is off. Re-running init is a plausible way
			// to turn it back on, so do not stand in the way.
			name:  "inference disabled",
			facts: func(f rerunFacts) rerunFacts { f.SubsystemState = signer.SubsystemStateDisabled; return f },
			want:  false,
		},
		{
			// A daemon that could not answer leaves the facts empty.
			name:  "nothing known about the host",
			facts: func(rerunFacts) rerunFacts { return rerunFacts{Interactive: true} },
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := askBeforeRerunningSetup(tc.facts(configured)); got != tc.want {
				t.Errorf("askBeforeRerunningSetup = %v, want %v", got, tc.want)
			}
		})
	}
}

func rerunScanner(in string) lineReader { return bufio.NewScanner(strings.NewReader(in)) }

// TestConfirmSetupRerun covers what each answer does. The default is the
// non-mutating one: #782's whole complaint is that pressing Enter through
// a re-run reconfigured a working host.
func TestConfirmSetupRerun(t *testing.T) {
	configured := rerunFacts{
		Interactive:     true,
		HasModelHistory: true,
		SubsystemState:  signer.SubsystemStateReady,
		ActiveModelID:   "qwen3.5-2b",
	}

	t.Run("enter alone stops the re-run", func(t *testing.T) {
		var out bytes.Buffer
		if confirmSetupRerun(&out, rerunScanner("\n"), configured) {
			t.Error("bare Enter continued into the install conversation")
		}
		if !strings.Contains(out.String(), rerunDeclinedLine) {
			t.Errorf("output did not say the device was left alone:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "[y/N] (default: No)") {
			t.Errorf("the prompt did not show a No default:\n%s", out.String())
		}
	})

	t.Run("an exhausted stdin stops the re-run", func(t *testing.T) {
		var out bytes.Buffer
		if confirmSetupRerun(&out, rerunScanner(""), configured) {
			t.Error("EOF continued into the install conversation")
		}
	})

	t.Run("yes continues", func(t *testing.T) {
		var out bytes.Buffer
		if !confirmSetupRerun(&out, rerunScanner("y\n"), configured) {
			t.Error("y did not continue into the install conversation")
		}
		if strings.Contains(out.String(), rerunDeclinedLine) {
			t.Errorf("said it was leaving the device alone after a yes:\n%s", out.String())
		}
	})

	t.Run("a host the gate does not apply to is never asked", func(t *testing.T) {
		var out bytes.Buffer
		fresh := rerunFacts{Interactive: true}
		if !confirmSetupRerun(&out, rerunScanner(""), fresh) {
			t.Error("a first install was stopped by the re-run gate")
		}
		if out.Len() != 0 {
			t.Errorf("a first install saw the question:\n%s", out.String())
		}
	})
}

// TestRunInitViaDaemon_ConfiguredHostIsAskedBeforeTheReplay is the wiring:
// the two functions above are reachable from the real flow, and answering
// No ends the run before anything can change the host.
//
// It is the #782 scenario end to end — enrolled, model question answered,
// serving — and what it asserts is an ABSENCE: none of the steps that
// re-ask or re-measure may print anything.
func TestRunInitViaDaemon_ConfiguredHostIsAskedBeforeTheReplay(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	shrinkLoginTimers(t, 20*time.Millisecond)
	owner, keys := scriptStdinPipe(t)
	// Enter alone, before the run starts: the gate is the first prompt.
	if _, err := keys.Write([]byte("\n")); err != nil {
		t.Fatalf("script stdin: %v", err)
	}
	d := &promptsDaemon{
		statusSeq: []management.InferenceStatus{readyStatus()},
		catalog: &catalogDetailResp{
			ModelQuestionAnswered: true,
			Families:              []catalogDetailFamily{{ModelID: bundledModel, Active: true, Downloaded: true}},
		},
		setupState: management.SetupStateResponse{EngineInstalled: true, DesiredEngine: "ollama"},
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{})

	if !strings.Contains(out, "Run setup again?") {
		t.Fatalf("a configured, serving host was replayed without being asked\n---\n%s", out)
	}
	if !strings.Contains(out, rerunDeclinedLine) {
		t.Errorf("the run did not say it was leaving the device alone\n---\n%s", out)
	}
	// Nothing downstream may have run: these are the steps whose defaults
	// reconfigured a working host.
	for _, unwanted := range []string{
		"Coding-agent integration",
		"Local inference is slow",
		"Keep local inference on anyway?",
		"Choose the model for this computer",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("declining the re-run still reached %q\n---\n%s", unwanted, out)
		}
	}
}

// TestRunInitViaDaemon_ExpiredSignInIsStillAsked: a run that had to
// re-authenticate is still asked before the setup conversation replays.
//
// waired-agent#803 changed what `Reauth` means on the way in. It used to
// be the --force-reauth flag and nothing else, because `daemonIdentity`
// read /waired/v1/identity over plain TCP, that route is not in the
// daemon's read allow-list, and the 403 made the view nil on every host —
// so `reauthWanted`'s second arm never fired. With the read moved onto the
// socket it fires for real: a plain `waired init` on a host whose sign-in
// expired now arrives with Reauth set, no flag given.
//
// That is the daemon reporting a fact, not the operator asking for the
// host to be reconfigured, and the two must not be read as the same
// answer. Re-authentication has already completed by the time this gate
// is reached (it happens in the sign-in loop above), so declining leaves
// the host authenticated and otherwise untouched — which is what
// main.go's own `reauth && renewing` branch says an auth-only refresh
// should do: "whatever hardware / integration state is already on disk
// stays untouched".
func TestRunInitViaDaemon_ExpiredSignInIsStillAsked(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	shrinkSetupTimers(t)
	shrinkLoginTimers(t, 20*time.Millisecond)
	owner, keys := scriptStdinPipe(t)
	if _, err := keys.Write([]byte("\n")); err != nil {
		t.Fatalf("script stdin: %v", err)
	}
	d := &promptsDaemon{
		statusSeq: []management.InferenceStatus{readyStatus()},
		catalog: &catalogDetailResp{
			ModelQuestionAnswered: true,
			Families:              []catalogDetailFamily{{ModelID: bundledModel, Active: true, Downloaded: true}},
		},
		setupState: management.SetupStateResponse{EngineInstalled: true, DesiredEngine: "ollama"},
	}

	out := runDaemonInit(t, d.server(t).URL, owner, daemonInitScenario{reauth: true})

	if !strings.Contains(out, "Run setup again?") {
		t.Fatalf("a re-auth run replayed the setup conversation without asking\n---\n%s", out)
	}
	for _, unwanted := range []string{
		"Coding-agent integration",
		"Local inference is slow",
		"Keep local inference on anyway?",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("declining after a re-auth still reached %q\n---\n%s", unwanted, out)
		}
	}
}

// TestRerunGateLines pins the wording. Owner-approved copy: #599 states
// that new user-facing copy on this path is draft until it is approved.
func TestRerunGateLines(t *testing.T) {
	intro, question := rerunGateLines("Qwen3.5 2B")
	if intro != "This device is already set up — Qwen3.5 2B is serving here." {
		t.Errorf("intro = %q", intro)
	}
	want := "Run setup again? It re-asks every question and re-measures this computer.\n" +
		"  No leaves this device exactly as it is."
	if question != want {
		t.Errorf("question = %q, want %q", question, want)
	}
}
