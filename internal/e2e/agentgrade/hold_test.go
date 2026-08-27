//go:build e2e

// Hold the probe stack up so a real client can be pointed at it.
//
// TestAgentGrade answers "what does OUR classifier make of this model".
// It cannot answer "would Claude Code accept and execute what comes
// back" — that needs a usable tool_use id, arguments the tool's own
// schema admits, and on SSE a frame sequence the client's incremental
// parser folds. Asserting those from inside the probe only ever proves
// that our reader agrees with our writer; running the actual client
// against the actual gateway is the one check that cannot be wrong
// about what the client accepts.
//
// It found something the probe could not see: with the engine's tool
// parser bypassed, the recovered calls drove real Claude Code through
// Grep and Read, while the visible assistant text carried the model's
// raw chain-of-thought and a bare `</think>` (#426, #442).
//
//	WAIRED_HOLD_URL_FILE=/tmp/gw.url WAIRED_HOLD_SECONDS=900 \
//	WAIRED_AGENTGRADE_MODEL=<tag> \
//	  go test -tags e2e -run TestHoldStack -timeout 30m ./internal/e2e/agentgrade/...
//
// then, from another shell:
//
//	ANTHROPIC_BASE_URL="$(cat /tmp/gw.url)" ANTHROPIC_API_KEY=dummy \
//	  claude --print --model waired/test --allowedTools Read,Grep,Glob \
//	    --output-format stream-json --verbose "…" < /dev/null
//
// Warm the weights with one throwaway request first: a client's own
// timeout can expire while the engine is still loading the model, which
// looks exactly like the model failing to answer.
//
// Skips unless WAIRED_HOLD_URL_FILE is set, so an ordinary e2e run never
// parks a GPU for a quarter of an hour. Deleting the file releases it
// early.
package agentgrade_e2e

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHoldStack(t *testing.T) {
	urlFile := strings.TrimSpace(os.Getenv("WAIRED_HOLD_URL_FILE"))
	if urlFile == "" {
		t.Skip("set WAIRED_HOLD_URL_FILE to hold the stack for a real client")
	}
	bin, err := exec.LookPath("ollama")
	if err != nil {
		t.Skipf("ollama not installed: %v", err)
	}

	hold := 900
	if v := strings.TrimSpace(os.Getenv("WAIRED_HOLD_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("WAIRED_HOLD_SECONDS=%q is not a positive integer", v)
		}
		hold = n
	}

	// The stack's own deadline outlives the hold, so a client still
	// mid-request when the hold expires gets its answer rather than a
	// severed connection it would report as a model failure.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(hold+120)*time.Second)
	defer cancel()

	base := startStack(t, ctx, bin, modelTag()).Anthropic
	if err := os.WriteFile(urlFile, []byte(base), 0o644); err != nil {
		t.Fatalf("write %s: %v", urlFile, err)
	}
	t.Cleanup(func() { _ = os.Remove(urlFile) })
	t.Logf("holding %s at %s for up to %ds (delete the file to release)", modelTag(), base, hold)

	deadline := time.Now().Add(time.Duration(hold) * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(urlFile); os.IsNotExist(err) {
			t.Log("url file removed; releasing the stack")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
