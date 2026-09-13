package controlclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// apiError is the control plane's error envelope,
// {"error":{"type":…,"message":…,"hint":…}}.
type apiError struct {
	Type    string
	Message string
	Hint    string
}

// decodeAPIError reads the control plane's error envelope out of a response
// body. ok is false when the body is not that envelope. Whitespace runs in
// the message and hint collapse to single spaces, so a rendered error stays
// on one line.
func decodeAPIError(body []byte) (apiError, bool) {
	var env struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return apiError{}, false
	}
	return apiError{
		Type:    env.Error.Type,
		Message: strings.Join(strings.Fields(env.Error.Message), " "),
		Hint:    strings.Join(strings.Fields(env.Error.Hint), " "),
	}, true
}

// statusText renders a non-200 control-plane response as "status %d: "
// followed by what the control plane said. When the body is the error
// envelope that is its message, type and hint — "status 403: this device
// is already enrolled to a different account (account_mismatch); <hint>" —
// rather than the raw JSON (waired#1395). Any other body is printed as it
// came, trimmed. The message text is kept verbatim: classifyAuthKeyError
// recognises an old control plane by it.
func statusText(status int, body []byte) string {
	if ae, ok := decodeAPIError(body); ok && ae.Message != "" {
		msg := fmt.Sprintf("status %d: %s", status, ae.Message)
		if ae.Type != "" {
			msg += " (" + ae.Type + ")"
		}
		if ae.Hint != "" {
			msg += "; " + ae.Hint
		}
		return msg
	}
	return fmt.Sprintf("status %d: %s", status, bytes.TrimSpace(body))
}
