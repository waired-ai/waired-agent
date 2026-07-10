package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newInferCmd() *cobra.Command {
	var mgmt, gateway, model string
	var explain, asJSON bool
	cmd := &cobra.Command{
		Use:   `infer "<prompt>"`,
		Short: "Run a one-shot inference request through the Local Gateway (use --explain for an Auto Selector dry-run).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if explain {
				return runInferExplain(mgmt, model)
			}
			if len(args) < 1 {
				return errors.New(`usage: waired infer "<prompt>"`)
			}
			prompt := strings.Join(args, " ")
			return runInferChat(gateway, model, prompt, asJSON)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addGatewayFlag(cmd, &gateway)
	cmd.Flags().StringVar(&model, "model", "waired/default", "model alias or model_id")
	cmd.Flags().BoolVar(&explain, "explain", false, "show the auto-selector decision and exit (no inference)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "stream raw OpenAI SSE chunks instead of plain text")
	return cmd
}

// runInferExplain calls /waired/v1/inference/select and pretty-prints
// the Selection so the user can see why a particular endpoint won.
func runInferExplain(mgmt, model string) error {
	body, _ := json.Marshal(map[string]string{"model": model})
	resp, err := httpPost(mgmt+"/waired/v1/inference/select", body)
	if err != nil {
		return err
	}
	var sel struct {
		EndpointID    string `json:"endpoint_id"`
		ModelID       string `json:"model_id"`
		VariantID     string `json:"variant_id"`
		Runtime       string `json:"runtime"`
		EngineModel   string `json:"engine_model"`
		ExecutionMode string `json:"execution_mode"`
		Decision      struct {
			Reason []string `json:"reason"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(resp, &sel); err != nil {
		// fall back to raw print
		fmt.Println(string(resp))
		return nil
	}
	fmt.Printf("endpoint:     %s\n", sel.EndpointID)
	fmt.Printf("model_id:     %s\n", sel.ModelID)
	fmt.Printf("variant_id:   %s\n", sel.VariantID)
	fmt.Printf("runtime:      %s\n", sel.Runtime)
	fmt.Printf("engine_model: %s\n", sel.EngineModel)
	fmt.Printf("execution:    %s\n", sel.ExecutionMode)
	fmt.Println("reason:")
	for _, r := range sel.Decision.Reason {
		fmt.Printf("  - %s\n", r)
	}
	return nil
}

// runInferChat sends a streaming chat completion to the Local Gateway
// and prints token deltas to stdout. Wraps the prompt as a single
// user message; intended for quick smoke testing, not for tool-use
// or multi-turn conversations.
func runInferChat(gateway, model, prompt string, raw bool) error {
	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	req, err := http.NewRequest(http.MethodPost, gateway+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := readGatewayToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("gateway returned %d: %s\n(the %s gateway requires the Bearer token from <state-dir>/secrets/gateway-token; use the default %s gateway, or run with enough privilege to read the token)",
				resp.StatusCode, errBody, gateway, defaultInferGatewayURL)
		}
		return fmt.Errorf("gateway returned %d: %s", resp.StatusCode, errBody)
	}
	return streamChatResponse(resp.Body, raw)
}

// readGatewayToken best-effort loads the gateway Bearer token from the
// resolved state dir so an explicit --gateway pointed at the token-gated
// :9473 still works when the token is readable (root / user-mode installs).
// The default :9479 gateway ignores the header, so attaching it
// unconditionally is harmless. Never creates the file; "" means "send no
// Authorization header".
func readGatewayToken() string {
	data, err := os.ReadFile(filepath.Join(defaultStateDir(), "secrets", "gateway-token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// streamChatResponse decodes an OpenAI SSE stream and writes token
// deltas to stdout (or the raw chunk JSON if `raw`).
func streamChatResponse(r io.Reader, raw bool) error {
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			_, _ = fmt.Fprintln(out)
			return nil
		}
		if raw {
			_, _ = fmt.Fprintln(out, payload)
			_ = out.Flush()
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				_, _ = fmt.Fprint(out, c.Delta.Content)
				_ = out.Flush()
			}
		}
	}
	return scanner.Err()
}
