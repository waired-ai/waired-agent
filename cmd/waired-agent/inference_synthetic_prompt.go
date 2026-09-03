// The synthetic prompt every engine measurement on this host is built
// from.
//
// Shape and calibration come from the #625 harness (docs/reports/
// 20260704-mtp-vs-spill-24gb.md, private monorepo): numbered filler lines
// over a NATO-alphabet vocabulary, led by a caller-supplied nonce so two
// prompts never share a prefix the engine's cache could answer from.
//
// Two callers, and they want different things from it. The #1127 prefill
// ladder asks for a LINE count and reads the real depth back off the
// engine's counters; the #496 host-cutoff probe asks for a TOKEN target
// against a calibration it measured a moment earlier. Keeping the two
// entry points apart is what lets either of them correct its own
// estimate.
package main

import (
	"bytes"
	"fmt"
)

// syntheticPromptWords are the subsystem fillers. Kept as the #625
// harness wrote them so its tokens-per-line calibration carries over.
var syntheticPromptWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf",
	"hotel", "india", "juliet", "kilo", "lima", "mike", "november",
	"oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform",
	"victor", "whiskey", "xray", "yankee", "zulu",
}

// syntheticPrompt builds a ~targetTokens prompt of numbered filler
// lines. The nonce leads every line so runs never share a prefix.
//
// tokensPerLine is the caller's calibration, not a constant, because the
// line below tokenizes differently per model family: the #625 harness
// measured 35 on its anchor model, while the #496 cutoff's probe model
// measures 19.2 on the same text. Baking one in silently produced a
// prompt at 55 % of the requested depth.
func syntheticPrompt(targetTokens, tokensPerLine int, nonce string) string {
	if tokensPerLine <= 0 {
		tokensPerLine = 1
	}
	return syntheticPromptLines((targetTokens+tokensPerLine-1)/tokensPerLine, nonce)
}

// syntheticPromptLines builds the same prompt from an exact LINE count.
//
// The line count is what a caller controls; the token count is what the
// model decides, and only the model can say what the exchange rate is
// (#496's probe measures it rather than assuming — see
// calibrateHostCutoffPrompt). Splitting the two apart is what lets a
// caller read a prefill count back and correct its own estimate.
func syntheticPromptLines(lines int, nonce string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "session %s log begin\n", nonce)
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "entry %s-%06d: subsystem %s reported state %d with latency %d ms and checksum %d\n",
			nonce, i, syntheticPromptWords[i%len(syntheticPromptWords)], i%7, (i*13)%997, (i*31+7)%65521)
	}
	b.WriteString("Question: summarize the three most frequent subsystems above in one short paragraph.")
	return b.String()
}
