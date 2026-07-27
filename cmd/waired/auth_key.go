package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Auth keys (#175, waired#976) — the credential a headless host presents
// instead of a person completing a browser sign-in. The key is minted in
// the Waired console (or by CI's dev-only issuer) and redeemed by the
// daemon; `waired init` only carries it.

// authKeyFilePrefix marks a path instead of a literal key, matching
// Tailscale's `tailscale up --authkey file:...`.
const authKeyFilePrefix = "file:"

// authKeyEnv is the environment fallback, which is what a container uses:
// the key never appears in the process's argv, so it stays out of `ps`
// and out of any shell history.
const authKeyEnv = "WAIRED_AUTH_KEY"

// resolveAuthKey turns the --auth-key value (plus the environment) into
// the literal key to present, or "" when the operator supplied none.
//
// Three accepted forms, in precedence order:
//
//	--auth-key waired_ak_...        the key itself
//	--auth-key file:/path/to/key    read it from a file
//	(flag omitted)                  $WAIRED_AUTH_KEY, in either form
//
// readFile is injected so the file leg is testable without touching disk
// (CLAUDE.md §Test discipline: the seam sits below the behaviour, and the
// fake takes the real argument).
//
// Surrounding whitespace is always trimmed: a key written with
// `echo ... > key` carries a trailing newline, and a key that fails only
// because of an invisible character is a miserable thing to debug.
func resolveAuthKey(flagVal, env string, readFile func(string) ([]byte, error)) (string, error) {
	raw := strings.TrimSpace(flagVal)
	source := "--auth-key"
	if raw == "" {
		raw = strings.TrimSpace(env)
		source = authKeyEnv
	}
	if raw == "" {
		return "", nil
	}

	if path, ok := strings.CutPrefix(raw, authKeyFilePrefix); ok {
		path = strings.TrimSpace(path)
		if path == "" {
			return "", fmt.Errorf("%s: %q gives no file path", source, raw)
		}
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: read %s: %w", source, path, err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("%s: %s is empty", source, path)
		}
		return key, nil
	}
	return raw, nil
}

// authKeyFromFlags resolves the flag against the real environment and
// filesystem.
func authKeyFromFlags(flagVal string) (string, error) {
	return resolveAuthKey(flagVal, os.Getenv(authKeyEnv), os.ReadFile)
}

// errAuthKeyUnsupported explains the one failure that looks like a bug in
// the key rather than in the deployment: a Control Plane predating auth
// keys rejects the unknown `auth_key` field outright (its JSON decoder
// runs with DisallowUnknownFields), so the operator sees a bare 400 about
// a field they were told to pass.
var errAuthKeyUnsupported = errors.New(
	"this control plane does not support auth keys yet.\n" +
		"  Auth keys need a control plane running waired#976 or newer.\n" +
		"  Sign in interactively instead:  waired init")

// classifyAuthKeyError maps a login-start failure into something the
// operator can act on. It only rewrites the two cases whose raw form is
// misleading; everything else is returned untouched.
func classifyAuthKeyError(err error, usedAuthKey bool) error {
	if err == nil || !usedAuthKey {
		return err
	}
	msg := err.Error()
	// The old-CP 400. Match on the field name rather than the status so a
	// different 400 (a genuinely malformed request) still surfaces as
	// itself.
	if strings.Contains(msg, "auth_key") && strings.Contains(msg, "unknown field") {
		return errAuthKeyUnsupported
	}
	return err
}
