package controlclient

import (
	"encoding/json"
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
