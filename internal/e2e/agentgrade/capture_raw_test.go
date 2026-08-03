//go:build e2e

// Capture the RAW gateway turns a run produces, so the wire bytes can be
// checked against the Anthropic contract rather than against our own
// Classify.
//
// Classify answers "did a structured tool_use come back, naming a real
// tool, with arguments its schema admits". It cannot answer "is this
// turn well formed on the wire" — a usable id, block indices that open
// and close, `input_json_delta` fragments that concatenate to valid
// JSON, a stop_reason consistent with the blocks. Those are properties
// of the ENCODING, and a probe that decodes with our own reader can
// only ever confirm that our reader agrees with our writer.
//
// Pair with scripts/dev/agentgrade-contract.py, which reads what this
// writes:
//
//	WAIRED_AGENTGRADE_MODEL=<tag> WAIRED_CAPTURE_DIR=/tmp/cap \
//	WAIRED_CAPTURE_TRIALS=12 WAIRED_AGENTGRADE_NO_PULL=1 \
//	  go test -tags e2e -run TestCaptureRawTurns -timeout 60m ./internal/e2e/agentgrade/...
//	python3 scripts/dev/agentgrade-contract.py /tmp/cap
//
// Skips unless WAIRED_CAPTURE_DIR is set: an ordinary e2e run has no
// reason to write a few hundred files.
package agentgrade_e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
)

func TestCaptureRawTurns(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("WAIRED_CAPTURE_DIR"))
	if dir == "" {
		t.Skip("set WAIRED_CAPTURE_DIR to capture raw turns")
	}
	bin, err := exec.LookPath("ollama")
	if err != nil {
		t.Skipf("ollama not installed: %v", err)
	}

	n := agentgrade.DefaultTrials
	if v := strings.TrimSpace(os.Getenv("WAIRED_CAPTURE_TRIALS")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			t.Fatalf("WAIRED_CAPTURE_TRIALS=%q is not a positive integer", v)
		}
		n = parsed
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	base := startStack(t, ctx, bin, modelTag())
	url := strings.TrimRight(base, "/") + "/v1/messages"
	client := &http.Client{Timeout: 10 * time.Minute}

	// The offered tool set travels with the capture: the checker has to
	// validate arguments against the very schemas the model was shown,
	// and requiring it to import this package to get them would put a Go
	// build between a capture and a question about it.
	req0, err := agentgrade.BuildRequest("waired/test", agentgrade.Cases[0])
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	tools, err := json.MarshalIndent(req0.Tools, "", "  ")
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_tools.json"), tools, 0o644); err != nil {
		t.Fatalf("write _tools.json: %v", err)
	}

	// Every case, including the one that wants no tool call: the engine's
	// parser splits the reasoning channel as well as the tool calls, so
	// the pure-text turn is where chain-of-thought reaching the user
	// would first show up.
	for _, c := range agentgrade.Cases {
		for _, stream := range []bool{false, true} {
			label := "unary"
			if stream {
				label = "stream"
			}
			for i := 0; i < n; i++ {
				req, err := agentgrade.BuildRequest("waired/test", c)
				if err != nil {
					t.Fatalf("BuildRequest: %v", err)
				}
				req.Stream = stream
				body, err := json.Marshal(req)
				if err != nil {
					t.Fatalf("marshal request: %v", err)
				}
				hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				hreq.Header.Set("Content-Type", "application/json")
				hreq.Header.Set("anthropic-version", "2023-06-01")

				stem := filepath.Join(dir, fmt.Sprintf("%s.%s.%02d", c.Name, label, i))
				resp, err := client.Do(hreq)
				if err != nil {
					// Kept as a file rather than failing the run: a
					// transport error is itself an observation, and
					// aborting here would discard the turns already
					// captured.
					if werr := os.WriteFile(stem+".transport-error", []byte(err.Error()), 0o644); werr != nil {
						t.Fatalf("write transport error: %v", werr)
					}
					t.Logf("%s: transport error: %v", filepath.Base(stem), err)
					continue
				}
				raw, rerr := io.ReadAll(resp.Body)
				if cerr := resp.Body.Close(); cerr != nil {
					t.Logf("%s: close body: %v", filepath.Base(stem), cerr)
				}
				if rerr != nil {
					t.Fatalf("read body: %v", rerr)
				}
				// The status is in the NAME because a turn cannot be
				// judged without it: an engine parse failure arrives as
				// a 500 whose body is not a turn at all.
				out := fmt.Sprintf("%s.%d.raw", stem, resp.StatusCode)
				if err := os.WriteFile(out, raw, 0o644); err != nil {
					t.Fatalf("write %s: %v", out, err)
				}
			}
			t.Logf("captured %s %s ×%d", c.Name, label, n)
		}
	}
	t.Logf("check it with: python3 scripts/dev/agentgrade-contract.py %s", dir)
}
