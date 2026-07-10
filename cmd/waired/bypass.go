package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/gcptoken"
)

// bypassCompleteLogin POSTs JSON to {control}/test/complete-login,
// flipping a waiting login session to "authorized" by issuing a mock id
// token bound to the supplied email. Mirrors the test helper at
// internal/controlplane/api/api_test.go:110.
func bypassCompleteLogin(ctx context.Context, c *http.Client, controlURL, sessionID, email string) error {
	body, _ := json.Marshal(map[string]string{
		"login_session_id": sessionID,
		"email":            email,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", controlURL+"/test/complete-login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return &completionError{status: resp.StatusCode, msg: string(buf)}
	}
	return nil
}

// lastPathSegment returns the substring after the final '/' in s, or ""
// if there is no slash.
func lastPathSegment(s string) string {
	i := strings.LastIndexByte(s, '/')
	if i < 0 {
		return ""
	}
	return s[i+1:]
}

// bypassHTTPClient returns an *http.Client suitable for talking to a
// `--bypass-idp` Control Plane. When the GCE metadata server is
// reachable, the client transparently injects a Google identity token
// (audience = controlURL) into the Authorization header so a
// Cloud Run / IAP IAM gate accepts the request. Off-GCE callers get a
// plain client (which assumes the bypass service is publicly callable).
func bypassHTTPClient(ctx context.Context, controlURL string) *http.Client {
	tr := gcptoken.New(controlURL, nil)
	if !tr.Probe(ctx) {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: tr}
}
