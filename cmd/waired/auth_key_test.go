package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolveAuthKey is the whole surface an operator can get wrong, so it is
// table-tested rather than exercised only through an enrollment.

func TestResolveAuthKey(t *testing.T) {
	const key = "waired_ak_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// readFile records what it was asked for: a fake that dropped the path
	// would make "reads the path it was given" untestable
	// (CLAUDE.md §Test discipline).
	var asked []string
	files := map[string]string{
		"/keys/prod":     key + "\n",
		"/keys/padded":   "  " + key + "  \n\n",
		"/keys/empty":    "   \n",
		"/keys/unwanted": "should-not-be-read",
	}
	readFile := func(path string) ([]byte, error) {
		asked = append(asked, path)
		v, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(v), nil
	}

	cases := []struct {
		name     string
		flag     string
		env      string
		want     string
		wantErr  bool
		wantRead []string
	}{
		// PRODUCT CONTRACT: all three input forms work, and the flag wins
		// over the environment.
		{name: "literal flag", flag: key, want: key},
		{name: "no key at all", want: ""},
		{name: "env fallback", env: key, want: key},
		{name: "flag beats env", flag: key, env: "waired_ak_other", want: key},
		{name: "file flag", flag: "file:/keys/prod", want: key, wantRead: []string{"/keys/prod"}},
		{name: "file via env", env: "file:/keys/prod", want: key, wantRead: []string{"/keys/prod"}},

		// PRODUCT CONTRACT: whitespace never reaches the wire. A key
		// written with `echo ... > key` carries a newline, and a key that
		// fails only because of an invisible character is miserable to
		// debug.
		{name: "trailing newline trimmed", flag: "file:/keys/padded", want: key, wantRead: []string{"/keys/padded"}},
		{name: "literal is trimmed", flag: "  " + key + "\n", want: key},
		{name: "whitespace-only env is no key", env: "   ", want: ""},

		// Failures must name the source, so the operator knows whether to
		// look at the flag or the environment.
		{name: "missing file", flag: "file:/keys/nope", wantErr: true, wantRead: []string{"/keys/nope"}},
		{name: "empty file", flag: "file:/keys/empty", wantErr: true, wantRead: []string{"/keys/empty"}},
		{name: "file: with no path", flag: "file:", wantErr: true},
		{name: "file: with blank path", flag: "file:   ", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			got, err := resolveAuthKey(tc.flag, tc.env, readFile)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got key %q", got)
				}
				if got != "" {
					t.Errorf("an error must not also yield a key, got %q", got)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Errorf("key = %q, want %q", got, tc.want)
				}
			}
			if len(asked) != len(tc.wantRead) {
				t.Fatalf("read %v, want %v", asked, tc.wantRead)
			}
			for i := range asked {
				if asked[i] != tc.wantRead[i] {
					t.Errorf("read[%d] = %q, want %q", i, asked[i], tc.wantRead[i])
				}
			}
		})
	}
}

func TestResolveAuthKey_ErrorNamesTheSource(t *testing.T) {
	// Record of today's behaviour, and worth keeping: with two possible
	// origins for the same value, a message that does not say which one it
	// read sends the operator to the wrong place.
	fail := func(string) ([]byte, error) { return nil, os.ErrNotExist }

	_, err := resolveAuthKey("file:/nope", "", fail)
	if err == nil || !strings.Contains(err.Error(), "--auth-key") {
		t.Errorf("flag error should name --auth-key, got %v", err)
	}
	_, err = resolveAuthKey("", "file:/nope", fail)
	if err == nil || !strings.Contains(err.Error(), authKeyEnv) {
		t.Errorf("env error should name %s, got %v", authKeyEnv, err)
	}
}

func TestAuthKeyFromFlags_ReadsARealFile(t *testing.T) {
	// The `var xFn = realFn` corollary: resolveAuthKey is table-tested with
	// a fake, so the real os.ReadFile wiring needs one test of its own or
	// nothing ever calls it.
	const key = "waired_ak_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := filepath.Join(t.TempDir(), "authkey")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := authKeyFromFlags("file:" + path)
	if err != nil {
		t.Fatalf("authKeyFromFlags: %v", err)
	}
	if got != key {
		t.Errorf("key = %q, want %q", got, key)
	}
}

func TestClassifyAuthKeyError(t *testing.T) {
	// PRODUCT CONTRACT: the one failure that looks like a bad key but is
	// really an old control plane gets rewritten into an actionable
	// message; nothing else is touched.
	oldCP := errors.New(`start login via daemon: 400: {"error":{"type":"invalid_request","message":"json: unknown field \"auth_key\""}}`)
	other := errors.New("start login via daemon: 403: auth_key_expired")

	if got := classifyAuthKeyError(oldCP, true); !errors.Is(got, errAuthKeyUnsupported) {
		t.Errorf("old-CP 400 should map to errAuthKeyUnsupported, got %v", got)
	}
	if got := classifyAuthKeyError(other, true); !errors.Is(got, other) {
		t.Errorf("an unrelated failure must pass through, got %v", got)
	}
	// Without an auth key the rewrite must never fire — an interactive run
	// that happens to mention auth_key is not an old control plane.
	if got := classifyAuthKeyError(oldCP, false); !errors.Is(got, oldCP) {
		t.Errorf("no auth key means no rewrite, got %v", got)
	}
	if got := classifyAuthKeyError(nil, true); got != nil {
		t.Errorf("nil in, nil out; got %v", got)
	}
}
