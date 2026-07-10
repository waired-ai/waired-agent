package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// OIDC direct-grant client helpers (--google-sa-login). They complete a
// login session by presenting a Google-signed service-account ID token to
// the Control Plane's /v1/login/oidc-grant endpoint, so enrollment against
// a production-like CP (e.g. dev.waired.net with --enable-oidc-grant) needs
// no browser. See internal/controlplane/api/oidc_grant.go for the server
// side and the security model.

// oidcGrantCompleteLogin POSTs the ID token to {control}/v1/login/oidc-grant,
// flipping the waiting login session to "authorized". Mirrors
// bypassCompleteLogin (cmd/waired/bypass.go) for the OIDC-grant path.
func oidcGrantCompleteLogin(ctx context.Context, c *http.Client, controlURL, sessionID, idToken string) error {
	body, _ := json.Marshal(map[string]string{
		"login_session_id": sessionID,
		"id_token":         idToken,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", controlURL+"/v1/login/oidc-grant", bytes.NewReader(body))
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

// obtainSAIDToken returns the ID token to present. An explicit token wins
// (CI mints it out-of-band, e.g. after a Workload Identity Federation auth
// step). Otherwise it mints one via gcloud, discovering the audience from
// the CP when --oidc-audience was not supplied.
func obtainSAIDToken(ctx context.Context, c *http.Client, controlURL, explicitToken, sa, audience string) (string, error) {
	if explicitToken != "" {
		return explicitToken, nil
	}
	if sa == "" {
		return "", fmt.Errorf("no --oidc-id-token and no --impersonate-sa to mint one")
	}
	if audience == "" {
		aud, err := fetchOIDCAudience(ctx, c, controlURL)
		if err != nil {
			return "", fmt.Errorf("discover audience (pass --oidc-audience to skip): %w", err)
		}
		audience = aud
	}
	return mintSAIDToken(ctx, sa, audience)
}

// fetchOIDCAudience GETs the audience (Google OAuth client_id) the CP
// expects in a presented ID token. The client_id is not secret.
func fetchOIDCAudience(ctx context.Context, c *http.Client, controlURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", controlURL+"/v1/login/oidc-grant/audience", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, buf)
	}
	var out struct {
		Audience string `json:"audience"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&out); err != nil {
		return "", err
	}
	if out.Audience == "" {
		return "", fmt.Errorf("control plane returned an empty audience")
	}
	return out.Audience, nil
}

// mintSAIDToken shells out to gcloud to mint a Google-signed ID token for
// the impersonated service account, with email claims included and the
// audience pinned to the CP's OAuth client_id.
func mintSAIDToken(ctx context.Context, sa, audience string) (string, error) {
	mintCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(mintCtx, "gcloud", "auth", "print-identity-token",
		"--impersonate-service-account="+sa,
		"--audiences="+audience,
		"--include-email")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gcloud print-identity-token: %s", msg)
	}
	tok := strings.TrimSpace(stdout.String())
	if tok == "" {
		return "", fmt.Errorf("gcloud print-identity-token returned an empty token")
	}
	return tok, nil
}
