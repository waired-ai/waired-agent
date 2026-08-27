package intercept

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// passthroughWithNotice relays the fallback request to the real upstream like
// passthrough, but splices a short human-readable reroute notice into the
// response's Anthropic SSE stream (#757) so the user can tell in-conversation
// that waired rerouted the turn. Only the per-request ReverseProxy copy gets
// ModifyResponse — never the shared s.rp, which would leak the wrap onto every
// passthrough. Non-stream responses are left untouched (the tray/status record
// still fired via OnFallback).
func (s *Server) passthroughWithNotice(w http.ResponseWriter, r *http.Request, notice string) {
	rp := *s.rp
	rp.ModifyResponse = func(resp *http.Response) error {
		if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			return nil
		}
		resp.Body = newRerouteNoticeInjector(resp.Body, notice)
		return nil
	}
	rp.ServeHTTP(w, r)
}

// buildRerouteNotice composes the notice text for an auto-mode reroute. It is
// class-aware (a subagent turn is named so, since a subagent-side signal is
// otherwise invisible to the user) and adds the peer + budget detail for a
// TTFB-timeout reroute.
func buildRerouteNotice(class, localErr, peer, budgetMs string) string {
	turn := "this turn"
	if class == classSub {
		turn = "this subagent turn"
	}
	if localErr == localErrPinnedPeerUnreachable {
		// waired-agent#325: the pin is fail-closed, so an unreachable pinned
		// worker no longer quietly hands the turn to this machine's engine —
		// on the auto route it leaves for the Anthropic API instead. Say which
		// setting caused it; the peer is deliberately not named (an
		// identifier means nothing to the person reading the transcript).
		return fmt.Sprintf("\n\n---\n> ⚠️ waired: %s was rerouted to the Anthropic API because the worker "+
			"you pinned is unavailable. Change routing with `waired worker`.", turn)
	}
	if localErr == localErrEngineTTFBTimeout {
		// waired-agent#837: this computer's own engine produced nothing
		// before its budget elapsed — a cold model load is the usual
		// reason, but the only thing observed is the silence, so that is
		// all this says. No machine is named: it is the one the user is
		// sitting at.
		within := ""
		if b := budgetHuman(budgetMs); b != "" {
			within = " for " + b
		}
		return fmt.Sprintf("\n\n---\n> ⚠️ waired: %s was rerouted to the Anthropic API because the model on this "+
			"computer had not answered%s. Change routing with `waired claude route`.", turn, within)
	}
	if (localErr == localErrPeerStoppedServing || localErr == localErrPeerUnreachable) && peer != "" {
		// waired-agent#1040: the peer was asked, and answered. Say what it
		// said rather than "returned no response", which is what the flat
		// timeout below says and would be a false account of a machine that
		// reported it had stopped. The elapsed wait is named for the same
		// reason the budget is named there — a turn that left the mesh
		// after three minutes reads very differently from one that left
		// after twenty seconds.
		after := ""
		if b := budgetHuman(budgetMs); b != "" {
			after = " after " + b
		}
		what := "stopped working on it"
		if localErr == localErrPeerUnreachable {
			what = "stopped answering"
		}
		return fmt.Sprintf("\n\n---\n> ⚠️ waired: %s was routed to a mesh peer (%s) which %s%s, "+
			"so it was rerouted to the Anthropic API. Change routing with `waired claude route`.",
			turn, peer, what, after)
	}
	if localErr == localErrPeerTTFBTimeout && peer != "" {
		within := ""
		if b := budgetSeconds(budgetMs); b != "" {
			within = " within " + b
		}
		return fmt.Sprintf("\n\n---\n> ⚠️ waired: %s was routed to a mesh peer (%s) that returned no response%s, "+
			"so it was rerouted to the Anthropic API. Change routing with `waired claude route`.", turn, peer, within)
	}
	return fmt.Sprintf("\n\n---\n> ⚠️ waired: %s was rerouted to the Anthropic API because local/mesh serving "+
		"was unavailable. Change routing with `waired claude route`.", turn)
}

// budgetHuman renders a millisecond budget the way a person waiting would say
// it: whole minutes as "10 minutes", anything shorter as budgetSeconds does.
//
// A separate helper rather than a change to budgetSeconds, which renders the
// peer budgets (60s / 20s) in already-shipped copy. The local budget is ten
// minutes by default, and "600s" is not a length anyone reads as ten minutes.
func budgetHuman(ms string) string {
	n, err := strconv.Atoi(ms)
	if err != nil || n <= 0 {
		return ""
	}
	if n%60000 != 0 {
		return budgetSeconds(ms)
	}
	if m := n / 60000; m == 1 {
		return "1 minute"
	} else {
		return strconv.Itoa(m) + " minutes"
	}
}

// budgetSeconds renders a millisecond budget as a friendly duration ("20s",
// or "1500ms" when not a whole second). Empty for an unparseable/zero value.
func budgetSeconds(ms string) string {
	n, err := strconv.Atoi(ms)
	if err != nil || n <= 0 {
		return ""
	}
	if n%1000 == 0 {
		return strconv.Itoa(n/1000) + "s"
	}
	return strconv.Itoa(n) + "ms"
}

// newRerouteNoticeInjector wraps an Anthropic SSE response body and splices a
// trailing text content_block carrying `notice` immediately before the terminal
// message_delta event, so the notice renders as the last text of the (fallback)
// response. It is FAIL-OPEN by construction:
//
//   - if any content block is a tool_use (structured output the orchestrator
//     parses), injection is skipped entirely — trailing prose must never
//     corrupt a structured subagent result;
//   - if the message_delta boundary is never seen (truncated/odd stream), or on
//     any read error, the bytes forwarded so far are exactly the upstream's and
//     nothing is spliced.
//
// Lines are forwarded verbatim (including their exact terminators) as they
// arrive, so streaming is preserved and a non-injected stream is byte-identical
// to the upstream.
func newRerouteNoticeInjector(src io.ReadCloser, notice string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = src.Close() }()
		br := bufio.NewReaderSize(src, 64*1024)
		blocks := 0   // content_block_start events seen so far → next free index
		skip := false // a tool_use block was seen → do not inject
		injected := false
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				trimmed := bytes.TrimRight(line, "\r\n")
				if bytes.HasPrefix(trimmed, []byte("data:")) &&
					bytes.Contains(trimmed, []byte(`"type":"content_block_start"`)) {
					blocks++
					if bytes.Contains(trimmed, []byte(`"type":"tool_use"`)) {
						skip = true
					}
				}
				if !injected && !skip && bytes.Equal(trimmed, []byte("event: message_delta")) {
					injected = true
					if _, werr := pw.Write(noticeSSE(blocks, notice)); werr != nil {
						return
					}
				}
				if _, werr := pw.Write(line); werr != nil {
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					_ = pw.Close()
				} else {
					_ = pw.CloseWithError(err)
				}
				return
			}
		}
	}()
	return pr
}

// noticeSSE builds the three-event Anthropic SSE block (content_block_start /
// _delta / _stop) for a text block at the given index carrying notice. json
// marshalling handles all escaping of the notice text.
func noticeSSE(index int, notice string) []byte {
	var b bytes.Buffer
	emit := func(event string, payload map[string]any) {
		data, _ := json.Marshal(payload)
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteString("\ndata: ")
		b.Write(data)
		b.WriteString("\n\n")
	}
	emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": notice},
	})
	emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
	return b.Bytes()
}
