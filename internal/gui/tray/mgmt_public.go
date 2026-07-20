package tray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/waired-ai/waired-agent/internal/management"
)

// ErrPublicShareUnsupported is returned by the PublicShare* methods when
// the daemon predates the provider Public Share toggle (HTTP 404). The
// tray hides the "Share to public nodes" group rather than surfacing a
// generic error.
var ErrPublicShareUnsupported = errors.New("daemon does not expose public-share control; upgrade waired-agent")

// ErrPublicUseUnsupported is returned by the PublicUse / PublicWarning /
// consent methods when the daemon predates the consumer-side Public
// Share settings (HTTP 404). The tray hides the "Use public nodes"
// group rather than surfacing a generic error.
var ErrPublicUseUnsupported = errors.New("daemon does not expose public-use settings; upgrade waired-agent")

// ErrPublicConsentRequired is returned by SetPublicUse when the daemon
// rejects a settings change because the user has not yet accepted the
// current warning (HTTP 409, error code "consent_required"). The tray
// shows the warning dialog and, on acceptance, records consent before
// retrying.
var ErrPublicConsentRequired = errors.New("public use requires accepting the warning first")

// ErrPublicWarningVersionMismatch is returned by AcceptPublicConsent
// when the version the client consented to no longer matches the one
// the daemon serves (HTTP 409, error code "warning_version_mismatch").
// The tray re-fetches the warning text and asks again.
var ErrPublicWarningVersionMismatch = errors.New("public-share warning text changed; re-read it and consent again")

// PublicShareStatus fetches GET /waired/v1/public/share — the provider
// toggle's current + desired state and CP-sync status. 404 →
// ErrPublicShareUnsupported.
func (c *Client) PublicShareStatus(ctx context.Context) (*management.PublicShareStateResponse, error) {
	var s management.PublicShareStateResponse
	if err := c.getJSON(ctx, "/waired/v1/public/share", &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicShareUnsupported, nil)
	}
	return &s, nil
}

// EnablePublicShare POSTs /waired/v1/public/share/enable, opting this
// machine in to serving public nodes. maxClients caps concurrent guests;
// 0 keeps the Waired-service default. 404 → ErrPublicShareUnsupported.
func (c *Client) EnablePublicShare(ctx context.Context, maxClients int) (*management.PublicShareStateResponse, error) {
	// The server's request struct is unexported, so mirror its single
	// field with an anonymous struct carrying the same JSON tag.
	body := struct {
		MaxClients int `json:"max_clients"`
	}{MaxClients: maxClients}
	var s management.PublicShareStateResponse
	if err := c.postJSON(ctx, "/waired/v1/public/share/enable", body, &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicShareUnsupported, nil)
	}
	return &s, nil
}

// DisablePublicShare POSTs /waired/v1/public/share/disable, opting this
// machine out of serving public nodes. 404 → ErrPublicShareUnsupported.
func (c *Client) DisablePublicShare(ctx context.Context) (*management.PublicShareStateResponse, error) {
	var s management.PublicShareStateResponse
	if err := c.postJSON(ctx, "/waired/v1/public/share/disable", struct{}{}, &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicShareUnsupported, nil)
	}
	return &s, nil
}

// PublicUse fetches GET /waired/v1/public/use — the consumer-side
// settings (mode, tier threshold, class flags) plus the effective mode
// and consent status. 404 → ErrPublicUseUnsupported.
func (c *Client) PublicUse(ctx context.Context) (*management.PublicUseResponse, error) {
	var s management.PublicUseResponse
	if err := c.getJSON(ctx, "/waired/v1/public/use", &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicUseUnsupported, nil)
	}
	return &s, nil
}

// SetPublicUse POSTs /waired/v1/public/use with the supplied pointer
// fields (nil = leave unchanged) and returns the resulting settings.
// 404 → ErrPublicUseUnsupported; 409 "consent_required" →
// ErrPublicConsentRequired so the tray can prompt for consent first.
func (c *Client) SetPublicUse(ctx context.Context, req management.PublicUseUpdateRequest) (*management.PublicUseResponse, error) {
	var s management.PublicUseResponse
	if err := c.postJSON(ctx, "/waired/v1/public/use", req, &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicUseUnsupported, map[string]error{
			"consent_required": ErrPublicConsentRequired,
		})
	}
	return &s, nil
}

// PublicWarning fetches GET /waired/v1/public/warning — the single
// source of the consent warning (version, title, body, and the
// server-authored accept/cancel button labels the tray renders
// verbatim). 404 → ErrPublicUseUnsupported.
func (c *Client) PublicWarning(ctx context.Context) (*management.PublicWarningResponse, error) {
	var s management.PublicWarningResponse
	if err := c.getJSON(ctx, "/waired/v1/public/warning", &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicUseUnsupported, nil)
	}
	return &s, nil
}

// AcceptPublicConsent POSTs /waired/v1/public/consent recording that the
// user accepted the given warning version, and returns the resulting
// settings. 404 → ErrPublicUseUnsupported; 409 "warning_version_mismatch"
// → ErrPublicWarningVersionMismatch so the tray re-reads the text.
func (c *Client) AcceptPublicConsent(ctx context.Context, warningVersion int) (*management.PublicUseResponse, error) {
	var s management.PublicUseResponse
	req := management.PublicConsentRequest{WarningVersion: warningVersion}
	if err := c.postJSON(ctx, "/waired/v1/public/consent", req, &s); err != nil {
		return nil, mapPublicErr(err, ErrPublicUseUnsupported, map[string]error{
			"warning_version_mismatch": ErrPublicWarningVersionMismatch,
		})
	}
	return &s, nil
}

// mapPublicErr translates a Client transport error into a public
// share/use sentinel. A 404 becomes the supplied unsupported sentinel
// (the endpoint is absent on older daemons). When codeSentinels is
// non-empty, a non-2xx whose error code (see publicErrorCode) contains
// one of the keys maps to that sentinel. Anything else is returned
// verbatim, and non-httpError values (dial failures, context
// cancellation) pass straight through.
func mapPublicErr(err error, unsupported error, codeSentinels map[string]error) error {
	var hr *httpError
	if !errors.As(err, &hr) {
		return err
	}
	if hr.StatusCode == http.StatusNotFound {
		return unsupported
	}
	if len(codeSentinels) > 0 {
		code := publicErrorCode(hr)
		for want, sentinel := range codeSentinels {
			if strings.Contains(code, want) {
				return sentinel
			}
		}
	}
	return err
}

// publicErrorCode extracts a machine-readable error code from an
// httpError body, tolerating BOTH management error envelopes: the
// {"error_code":...} form used by public_use.go and the {"error":...}
// form used by public_share.go. When neither field is present it falls
// back to the raw body so a substring match can still fire.
func publicErrorCode(hr *httpError) string {
	if hr == nil {
		return ""
	}
	var env struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if json.Unmarshal([]byte(hr.Body), &env) == nil {
		if env.ErrorCode != "" {
			return env.ErrorCode
		}
		if env.Error != "" {
			return env.Error
		}
	}
	return hr.Body
}
