package gateway

import "strings"

// CutSSEData returns an SSE line's payload.
//
// The field separator is "data:"; a single space after it is part of the
// framing and is stripped, and any further space is data. This gateway's
// own writer emits "data: ", and so does every engine measured so far,
// but the space is optional in the format — and the two readers in this
// package disagreed about it: the Anthropic stream reader required it
// while the usage sniffer did not (usage.go). An engine that omitted it
// therefore produced a turn with no content whose token accounting was
// nevertheless correct: an answerless clean finish, which is the exact
// signature of a model failing mid-stream (#442). Our own reader would
// have been blamed for the model's turn.
func CutSSEData(line string) (string, bool) {
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimPrefix(payload, " "), true
}
