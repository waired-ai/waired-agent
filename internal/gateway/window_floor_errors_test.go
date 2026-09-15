package gateway

import (
	"regexp"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// contextOverflowPatterns is what OpenCode reads as "the prompt is too long
// for this model" — on a match it compacts the conversation and retries.
// Copied from OpenCode v1.18.30, packages/llm/src/provider-error.ts
// (`patterns`), with Claude Code's own "prompt is too long" among them. RE2
// accepts every one as written.
//
// A window refusal is not that: no prompt length fixes a row whose floor no
// computer meets, so a client that compacted on it would loop. Refresh this
// list when the OpenCode pin in docs-site moves.
var contextOverflowPatterns = []string{
	`(?i)prompt is too long`,
	`(?i)request_too_large`,
	`(?i)input is too long for requested model`,
	`(?i)exceeds the context window`,
	`(?i)exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`,
	`(?i)input token count.*exceeds the maximum`,
	`(?i)tokens in request more than max tokens allowed`,
	`(?i)maximum prompt length is \d+`,
	`(?i)reduce the length of the messages`,
	`(?i)maximum context length is \d+ tokens`,
	`(?i)exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`,
	`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`,
	`(?i)exceeds the limit of \d+`,
	`(?i)exceeds the available context size`,
	`(?i)greater than the context length`,
	`(?i)context window exceeds limit`,
	`(?i)exceeded model token limit`,
	`(?i)context[_ ]length[_ ]exceeded`,
	`(?i)request entity too large`,
	`(?i)context length is only \d+ tokens`,
	`(?i)input length.*exceeds.*context length`,
	`(?i)prompt too long; exceeded (?:max )?context length`,
	`(?i)too large for model with \d+ maximum context length`,
	`(?i)prompt has [\d,]+ tokens?, but the configured context size is [\d,]+ tokens?`,
	`(?i)model_context_window_exceeded`,
	`(?i)too many tokens`,
	`(?i)token limit exceeded`,
}

func TestWindowRefusalsAreNotReadAsContextOverflow(t *testing.T) {
	var messages []string
	for _, err := range []error{
		router.ErrNoEndpointForWindow,
		&router.WindowFloorError{Need: 200704},
		&router.WindowFloorError{Need: 1048576, Public: true},
		&router.WindowFloorError{Need: 1048576, Local: true, LocalWindow: 262144},
		&router.WindowFloorError{Need: 200704, Local: true},
	} {
		d := windowFloorDetail(err)
		messages = append(messages, d, sentence(d),
			failClosedMessage("", d), failClosedMessage(state.ClaudeClassSub, d), err.Error())
	}
	for _, reason := range []string{
		router.PinDeclinedWindow, router.PinDeclinedServeMain, router.PinDeclinedServeSub,
		router.PinDeclinedPublicShare, router.PinDeclinedUnknownModel, "",
	} {
		err := &router.PinnedPeerDeclinedError{PeerName: "linux-gpu", Reason: reason, Need: 200704, Declared: 131072}
		d := pinnedPeerDeclinedDetail(err)
		messages = append(messages, d, sentence(d), failClosedMessage("", d))
	}
	messages = append(messages, pinnedPeerDeclinedDetail(router.ErrPinnedPeerDeclined))

	for _, p := range contextOverflowPatterns {
		re := regexp.MustCompile(p)
		for _, m := range messages {
			if re.MatchString(m) {
				t.Errorf("%q matches the overflow pattern %s — a coding tool would compact and retry on it", m, p)
			}
		}
	}
}
