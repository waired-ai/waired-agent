package main

import (
	"fmt"
	"io"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// The re-run gate: one question, asked before `waired init` replays the
// install conversation on a host that is already set up and serving.
//
// Operators re-run init to CHECK. On rc9 that was not a read-only thing to
// do: the run re-benchmarks, and the speed gates it feeds answer their own
// question with a default that changes the host. Pressing Enter through
// them turned local inference off on a macOS host that was serving the
// mesh, and on Windows switched the serving model and then deleted the
// weights of the one it replaced — opposite directions, from the same
// gesture, because which gate fires depends on whether a lighter model
// exists (waired-ai/waired-agent#782).
//
// Answering "keep what I have" was not possible: both gates are yes/no and
// neither answer is "leave it alone".
//
// The gates themselves are deliberately untouched. The owner ruling on
// waired-ai/waired-agent#599 (2026-08-09) is that a re-run replays the
// whole conversation — "各種のベンチマークやゲートも新規インストールと同じ
// ように設定する" — and that what makes it safe is idempotence: the same
// engine and the same model chosen again must change nothing. A default
// that switches the model or disables inference is what breaks that
// clause, and the cheapest place to honour it is before the replay starts
// rather than inside every gate along the way.
//
// So: say what this device is doing, ask whether to run setup again, and
// default to not. Yes replays everything exactly as it does today.

// rerunFacts is what the gate decides on. A struct rather than five
// arguments so the decision can be table-tested without a daemon
// (CLAUDE.md §Test discipline: put the seam below the behaviour).
type rerunFacts struct {
	// Interactive is whether a person is there to answer at all.
	Interactive bool
	// ExplicitIntent is whether this invocation already said what it
	// wants — `--force-reauth`, `--model`, `--inference-enabled`. A flag
	// IS the answer to this question, and asking it again would be
	// asking twice.
	ExplicitIntent bool
	// HasModelHistory is hostHasModelHistory: this host has answered the
	// model question, or has a model of its own beyond the timing probe.
	// It is the existing "this is not a first install" predicate.
	HasModelHistory bool
	// SubsystemState and ActiveModelID come from /waired/v1/inference/status.
	SubsystemState string
	ActiveModelID  string
}

// askBeforeRerunningSetup reports whether to put the question.
//
// Narrow on purpose. The state it fires for is the one #782 describes — a
// host that is enrolled, has answered the model question, and is serving
// right now — and every other shape falls through to the flow that exists
// today. In particular a host mid-setup does not qualify, so the resume
// #313 prescribes for a stuck install is untouched: a stuck host is not
// serving, and often has no model at all.
func askBeforeRerunningSetup(f rerunFacts) bool {
	if !f.Interactive || f.ExplicitIntent {
		return false
	}
	if !f.HasModelHistory || f.ActiveModelID == "" {
		return false
	}
	return f.SubsystemState == signer.SubsystemStateReady
}

// rerunFactsFor asks the daemon what this host is doing.
//
// Both reads are best-effort: a daemon that cannot answer leaves the facts
// empty, askBeforeRerunningSetup says no, and init proceeds exactly as it
// did before this gate existed. A question we cannot justify asking is one
// we do not ask.
//
// It deliberately reads /inference/status and /inference/catalog rather
// than /identity: enrolment alone does not mean a host is set up and
// serving, which is the state this gate is about.
// Cheapest disqualifier first, and each read is skipped once the answer
// can no longer be yes. That is not only economy: /inference/status is
// polled by later steps that count their own reads, and a question we
// were never going to ask has no business consuming one.
func rerunFactsFor(mgmtURL string, interactive, explicitIntent bool) rerunFacts {
	f := rerunFacts{Interactive: interactive, ExplicitIntent: explicitIntent}
	if !interactive || explicitIntent {
		return f
	}
	cat, ok := fetchCatalogDetail(mgmtURL)
	if !ok || !hostHasModelHistory(cat) {
		return f
	}
	f.HasModelHistory = true
	if st, ok := fetchInferenceStatus(mgmtURL); ok {
		f.SubsystemState = st.SubsystemState
		if st.Active != nil {
			f.ActiveModelID = st.Active.ModelID
		}
	}
	return f
}

// rerunGateLines is the question, minus the "[y/N] (default: No)" hint
// ynAsk appends. Two lines in the shape the other two-part questions in
// this flow use (init_benchmark.go, init_host_speed.go): the question,
// then an indented line saying what the default does.
func rerunGateLines(modelLabel string) (intro, question string) {
	intro = fmt.Sprintf("This computer is already set up. %s is serving here.", modelLabel)
	question = "Run setup again? It asks every question again and re-measures this computer.\n" +
		"  No leaves this computer exactly as it is."
	return intro, question
}

const (
	rerunDeclinedLine = "Leaving this computer as it is. Nothing was changed."
	rerunStatusHint   = "Run `waired status` to see what this computer is doing."
)

// confirmSetupRerun puts the question and reports whether to go on with
// the install conversation.
//
// An exhausted stdin answers No, like every other prompt in this flow
// (init_benchmark.go's noAnswerKeeps): a pipe that ran out never asked for
// the host to be reconfigured, and the non-mutating reading is the honest
// one.
func confirmSetupRerun(out io.Writer, sc lineReader, f rerunFacts) bool {
	if !askBeforeRerunningSetup(f) {
		return true
	}
	intro, question := rerunGateLines(bundledModelLabelDefault(f.ActiveModelID))
	writePromptf(out, "\n%s\n", intro)
	if ynAsk(out, sc, question, false) == ynYes {
		return true
	}
	writePrompt(out, rerunDeclinedLine)
	writePrompt(out, rerunStatusHint)
	return false
}
