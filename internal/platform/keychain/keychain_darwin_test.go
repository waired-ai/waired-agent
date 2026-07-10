//go:build darwin

package keychain

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// withFakeSecurity swaps runSecurityFn for the duration of the test
// and restores it on cleanup. Tests record the argv they saw and
// return canned stdout/stderr/err.
func withFakeSecurity(t *testing.T, fn func([]string, []byte) ([]byte, []byte, error)) {
	t.Helper()
	orig := runSecurityFn
	runSecurityFn = fn
	t.Cleanup(func() { runSecurityFn = orig })
}

// withRootEuid makes useSystemKeychain() report root so the
// System-keychain routing can be exercised on a non-root CI host.
func withRootEuid(t *testing.T) {
	t.Helper()
	orig := geteuidFn
	geteuidFn = func() int { return 0 }
	t.Cleanup(func() { geteuidFn = orig })
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestSet_SystemKeychainWhenRoot asserts that as root the Set argv adds
// -A (no ACL prompt for the headless daemon) and targets the System
// keychain positional, instead of the per-binary -T login-keychain path.
func TestSet_SystemKeychainWhenRoot(t *testing.T) {
	withRootEuid(t)
	var gotArgs []string
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		gotArgs = append([]string(nil), args...)
		return nil, nil, nil
	})
	if err := New().Set(Item{Account: "waired", Service: "machine-key"}, []byte("k")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !containsArg(gotArgs, "-A") {
		t.Errorf("root Set should pass -A (allow any app); got %v", gotArgs)
	}
	if containsArg(gotArgs, "-T") {
		t.Errorf("root Set should not pass -T (login-keychain ACL); got %v", gotArgs)
	}
	if gotArgs[len(gotArgs)-1] != systemKeychainPath {
		t.Errorf("root Set should target %s as the last argv; got %v", systemKeychainPath, gotArgs)
	}
}

// TestReadPaths_SystemKeychainWhenRoot asserts Get/Delete/Exists append
// the System keychain positional when running as root.
func TestReadPaths_SystemKeychainWhenRoot(t *testing.T) {
	withRootEuid(t)
	var gotArgs []string
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		gotArgs = append([]string(nil), args...)
		// Return a valid codec token so Get decodes cleanly.
		return []byte(encodeSecret([]byte("v")) + "\n"), nil, nil
	})
	store := New()
	item := Item{Account: "waired", Service: "access-token"}
	if _, err := store.Get(item); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotArgs[len(gotArgs)-1] != systemKeychainPath {
		t.Errorf("root Get should target System keychain; got %v", gotArgs)
	}
	if err := store.Delete(item); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotArgs[len(gotArgs)-1] != systemKeychainPath {
		t.Errorf("root Delete should target System keychain; got %v", gotArgs)
	}
	if _, err := store.Exists(item); err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if gotArgs[len(gotArgs)-1] != systemKeychainPath {
		t.Errorf("root Exists should target System keychain; got %v", gotArgs)
	}
}

func TestSet_ArgvShape(t *testing.T) {
	var gotArgs []string
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		gotArgs = append([]string(nil), args...)
		return nil, nil, nil
	})

	store := New()
	if err := store.Set(Item{Account: "waired", Service: "device-key"},
		[]byte("super-secret")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The secret is stored as the base64 codec token, never as raw bytes
	// on argv (#512): `security -w` round-trips binary as hex and argv
	// cannot carry a NUL.
	// Non-root (the CI host): login keychain, per-binary -T ACL, no
	// explicit keychain positional.
	want := []string{
		"add-generic-password",
		"-a", "waired",
		"-s", "device-key",
		"-w", encodeSecret([]byte("super-secret")),
		"-U",
		"-T", "/usr/bin/security",
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("argv mismatch\n got=%v\nwant=%v", gotArgs, want)
	}
}

func TestGet_Roundtrip(t *testing.T) {
	val := []byte("super-secret")
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		// Simulate the codec token + trailing-newline shape that real
		// `security ... -w` emits for a value this backend wrote.
		return []byte(encodeSecret(val) + "\n"), nil, nil
	})

	store := New()
	got, err := store.Get(Item{Account: "waired", Service: "device-key"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("got %q, want %q (codec token must decode + CLI newline strip)", got, val)
	}
}

// TestGet_LegacyValueTreatedAsNotFound covers the migration path: a value
// without the codec prefix — a legacy raw-binary item (rendered by
// `security -w` as a prefix-less hex string) or any foreign item — must
// read back as ErrNotFound so securestore falls through to the
// authoritative file and re-migrates it (#512).
func TestGet_LegacyValueTreatedAsNotFound(t *testing.T) {
	cases := map[string]string{
		"legacy hex render of 64 binary bytes": // 128 lowercase hex chars
		"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" +
			"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		"foreign printable string": "eyJhbGciOiJFZERTQSJ9.payload.sig",
	}
	for name, stored := range cases {
		t.Run(name, func(t *testing.T) {
			withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
				return []byte(stored + "\n"), nil, nil
			})
			_, err := New().Get(Item{Account: "waired", Service: "legacy"})
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get legacy value: got %v, want ErrNotFound", err)
			}
		})
	}
}

// TestEncodeDecodeSecret_BinaryRoundtrip proves the codec round-trips
// arbitrary bytes — including the NUL and high bytes that broke the raw
// argv path (#512).
func TestEncodeDecodeSecret_BinaryRoundtrip(t *testing.T) {
	all256 := make([]byte, 256)
	for i := range all256 {
		all256[i] = byte(i)
	}
	cases := map[string][]byte{
		"empty":               {},
		"nul-containing key":  {0x00, 0x01, 0x00, 0xff, 0x7f, 0x80, 0x00},
		"all 256 byte values": all256,
		"printable":           []byte("super-secret"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			enc := encodeSecret(in)
			if enc[len(enc)-1] == '\n' {
				t.Fatalf("encoded token must not contain a newline: %q", enc)
			}
			for i := 0; i < len(enc); i++ {
				if enc[i] == 0x00 {
					t.Fatalf("encoded token must be NUL-free (argv-safe); has NUL at %d", i)
				}
			}
			out, ok := decodeSecret([]byte(enc))
			if !ok {
				t.Fatalf("decodeSecret(%q) reported not-ours; want ok", enc)
			}
			if !bytes.Equal(out, in) {
				t.Fatalf("round-trip mismatch: got %x, want %x", out, in)
			}
		})
	}
}

func TestGet_NotFound(t *testing.T) {
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		return nil,
			[]byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n"),
			errors.New("exit status 44")
	})

	store := New()
	_, err := store.Get(Item{Account: "waired", Service: "missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: got %v, want ErrNotFound", err)
	}
}

func TestDelete_NotFoundIsReported(t *testing.T) {
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		return nil,
			[]byte("security: -25300: The specified item could not be found in the keychain.\n"),
			errors.New("exit status 44")
	})

	store := New()
	if err := store.Delete(Item{Account: "waired", Service: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete: got %v, want ErrNotFound", err)
	}
}

func TestExists_HitAndMiss(t *testing.T) {
	cases := []struct {
		name    string
		stderr  []byte
		execErr error
		want    bool
	}{
		{"hit", nil, nil, true},
		{"miss", []byte("could not be found in the keychain"), errors.New("exit 44"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
				return nil, c.stderr, c.execErr
			})
			got, err := New().Exists(Item{Account: "waired", Service: "x"})
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if got != c.want {
				t.Errorf("Exists: got %v, want %v", got, c.want)
			}
		})
	}
}

func TestValidate_RejectsEmpty(t *testing.T) {
	store := New()
	if err := store.Set(Item{Account: "", Service: "x"}, nil); err == nil {
		t.Error("Set with empty Account: want error")
	}
	if err := store.Set(Item{Account: "x", Service: ""}, nil); err == nil {
		t.Error("Set with empty Service: want error")
	}
	if _, err := store.Get(Item{Account: "x"}); err == nil {
		t.Error("Get with empty Service: want error")
	}
}

func TestExists_PropagatesUnknownError(t *testing.T) {
	withFakeSecurity(t, func(args []string, _ []byte) ([]byte, []byte, error) {
		return nil, []byte("totally unexpected failure"), errors.New("exit status 99")
	})
	if _, err := New().Exists(Item{Account: "waired", Service: "x"}); err == nil ||
		errors.Is(err, ErrNotFound) {
		t.Fatalf("Exists: got %v, want non-NotFound error", err)
	}
}
