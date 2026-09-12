package main

import (
	"fmt"
	"io"
	"strings"
)

// A question `waired init` asked that nothing answered.
//
// "Nothing answered it" is narrower than "there was no terminal". An
// empty line IS an answer — a person pressed Enter, and printing a
// default is what invites that. A flag is an answer too, and is read
// before stdin. What this file is about is the third case: the read hit
// EOF, so no answer is coming and none ever was.
//
// The rule until now was to take the side with no side effect, quietly,
// and exit 0 (waired-agent#1048 for the engine install and the model
// picker, waired-agent#1070 for the coding-tool and Claude-routing
// writes). That got the FIRST half right — a closed pipe must not install
// an engine, start a multi-GB download, or write managed settings for
// every account on the machine — and the second half wrong. The screen
// said "[Y/n] (default: Yes)" and then did the opposite of Yes; the run
// finished on a ready box; and the exit code was 0, so a server build had
// no way to notice that the local inference it was installed for is off.
// Observed on the rc6 review's Windows host, over ssh with no pty
// (waired-agent#1300).
//
// Owner ruling 2026-09-12: keep the principle, drop the silence. The
// question stops the run and names the flag that answers it. The
// principle is what makes this safe to do at the question rather than up
// front on an isatty check — a scripted install that pipes its answers in
// has a terminal-less stdin and is answering perfectly well, and
// scripts/dev/lib/installtest-enroll.sh drives the model picker exactly
// that way.
type unansweredQuestion struct {
	// question is the prompt as it was printed, without its [Y/n] hint.
	question string
	// why says what this run therefore did not do, in the operator's
	// terms rather than the code's.
	why string
	// flags are the ways to answer it without a terminal, most useful
	// first.
	flags []string
}

// The three questions whose answer commits this computer to something.
//
// The questions NOT here are as much of the decision as the ones that
// are. A run stops where nobody answering means nobody DECIDED; where the
// no-answer path lands exactly where --non-interactive lands, there is
// nothing to stop for and stopping would fail an install that did what it
// documents. That covers:
//
//   - the model picker, whose no-answer keeps the model Waired chose for
//     this computer — which is what --non-interactive does, and what the
//     daemon's held fallback was going to do anyway;
//   - the step-down and upgrade offers after the benchmark, whose
//     no-answer keeps the model that is already running;
//   - the host-speed re-ask, whose no-answer keeps a toggle the operator
//     wrote by hand — deliberately matched to --non-interactive by
//     waired-agent#1071, which fixed the opposite bug.
func unansweredEngineInstall() unansweredQuestion {
	return unansweredQuestion{
		question: "Run models on this computer?",
		why:      "local inference was left exactly as it is, neither turned on nor off",
		flags: []string{
			"--inference-enabled=true   run models here",
			"--inference-enabled=false  don't, and say so",
			"--non-interactive          use this computer's hardware to decide",
		},
	}
}

func unansweredIntegration() unansweredQuestion {
	return unansweredQuestion{
		question: "Set up coding-agent integration?",
		why:      "this computer's coding tools were left unconfigured",
		flags: []string{
			"--non-interactive   set them up",
			"--skip-integration  leave them alone",
		},
	}
}

func unansweredClaudeRoute() unansweredQuestion {
	return unansweredQuestion{
		question: "Route Claude Code inference through Waired now?",
		why:      "Claude Code on this computer still goes to the Anthropic API",
		flags: []string{
			"--non-interactive     route it",
			"--skip-claude-route   leave it on the Anthropic API",
		},
	}
}

// printNoAnswerStop is what the terminal says where the answer should
// have been. It replaces the old "No answer on stdin. Nobody is here to
// …" line, which said who was missing but not what to do about it.
func printNoAnswerStop(out io.Writer, q unansweredQuestion) {
	writePrompt(out)
	writePromptf(out, "%s No answer on stdin, so %q went unanswered.\n", emo("⚠", "!"), q.question)
	writePromptf(out, "  Waired stopped rather than choose for you, so %s.\n", q.why)
	writePrompt(out, "  Answer it without a terminal with one of:")
	for _, f := range q.flags {
		writePromptf(out, "    %s\n", cyan(f))
	}
}

// noAnswerBoxLines is the same set of questions as the closing box's rows.
// The box is what an operator reads when the run has scrolled past, and
// what makes a non-zero exit legible without going back through the log.
func noAnswerBoxLines(qs []unansweredQuestion) []string {
	lines := make([]string, 0, len(qs)+1)
	for _, q := range qs {
		lines = append(lines, fmt.Sprintf("  %s  %s", emo("·", "-"), q.question))
	}
	return lines
}

// noAnswerFlagSummary is the one line of remedy under those rows: the
// flags from every unanswered question, deduplicated, in the order they
// were first offered.
func noAnswerFlagSummary(qs []unansweredQuestion) string {
	var (
		seen  = map[string]bool{}
		names []string
	)
	for _, q := range qs {
		for _, f := range q.flags {
			name, _, _ := strings.Cut(f, " ")
			if !strings.HasPrefix(name, "-") || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return strings.Join(names, "  ")
}
