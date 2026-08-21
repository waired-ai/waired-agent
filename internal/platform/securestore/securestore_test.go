package securestore

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/keychain"
)

var testItem = keychain.Item{Account: Account, Service: ServiceMachineKey}

// fakeStore is a configurable keychain.Store for the error-path tests.
// A nil *Err means "behave like an in-memory store"; a non-nil one is
// returned instead of touching the backing map.
type fakeStore struct {
	data      map[keychain.Item][]byte
	getErr    error
	setErr    error
	deleteErr error
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[keychain.Item][]byte{}} }

func (f *fakeStore) Set(item keychain.Item, data []byte) error {
	if f.setErr != nil {
		return f.setErr
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.data[item] = cp
	return nil
}

func (f *fakeStore) Get(item keychain.Item) ([]byte, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.data[item]
	if !ok {
		return nil, keychain.ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) Delete(item keychain.Item) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.data[item]; !ok {
		return keychain.ErrNotFound
	}
	delete(f.data, item)
	return nil
}

func (f *fakeStore) Exists(item keychain.Item) (bool, error) {
	_, ok := f.data[item]
	return ok, nil
}

func use(t *testing.T, s keychain.Store) {
	t.Helper()
	t.Cleanup(SwapStoreForTest(s))
}

func TestRead_KeychainHit(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Set(testItem, []byte("from-keychain"))
	use(t, fs)

	// File is absent on purpose: a Keychain hit must not touch disk.
	got, err := Read(testItem, filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "from-keychain" {
		t.Fatalf("got %q, want from-keychain", got)
	}
}

func TestRead_MissFallsBackToFileAndMigrates(t *testing.T) {
	fs := newFakeStore() // empty => Get returns ErrNotFound
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("on-disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(testItem, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "on-disk" {
		t.Fatalf("got %q, want on-disk", got)
	}
	// Opportunistic migration: the file secret should now be in the store.
	if v, err := fs.Get(testItem); err != nil || string(v) != "on-disk" {
		t.Fatalf("expected opportunistic migration into keychain, got %q err=%v", v, err)
	}
}

func TestRead_UnsupportedFallsBackWithoutMigrating(t *testing.T) {
	fs := newFakeStore()
	fs.getErr = keychain.ErrUnsupported
	fs.setErr = keychain.ErrUnsupported
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("linux-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(testItem, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "linux-file" {
		t.Fatalf("got %q, want linux-file", got)
	}
	if len(fs.data) != 0 {
		t.Fatalf("ErrUnsupported must not migrate into keychain, store has %d items", len(fs.data))
	}
}

func TestRead_OtherErrorFallsBackToFile(t *testing.T) {
	fs := newFakeStore()
	fs.getErr = errors.New("keychain locked")
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("still-readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(testItem, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "still-readable" {
		t.Fatalf("got %q, want still-readable", got)
	}
}

func TestRead_BothMissingIsNotExist(t *testing.T) {
	use(t, newFakeStore())
	_, err := Read(testItem, filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("expected error when both keychain and file are absent")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("error must satisfy os.IsNotExist, got %v", err)
	}
}

func TestWrite_WritesBothAndFileMode(t *testing.T) {
	fs := newFakeStore()
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := Write(testItem, path, []byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// File written with payload.
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "payload" {
		t.Fatalf("file: got %q err=%v", b, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	// Keychain written too.
	if v, err := fs.Get(testItem); err != nil || string(v) != "payload" {
		t.Fatalf("keychain: got %q err=%v", v, err)
	}
}

func TestWrite_UnsupportedKeychainStillWritesFile(t *testing.T) {
	fs := newFakeStore()
	fs.setErr = keychain.ErrUnsupported
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := Write(testItem, path, []byte("payload")); err != nil {
		t.Fatalf("Write should not fail when keychain is unsupported: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "payload" {
		t.Fatalf("file: got %q err=%v", b, err)
	}
	if len(fs.data) != 0 {
		t.Fatalf("unsupported keychain must not store, has %d items", len(fs.data))
	}
}

func TestWrite_KeychainErrorClearsStaleEntry(t *testing.T) {
	fs := newFakeStore()
	// Seed a STALE keychain value, then make Set fail with a non-sentinel
	// error. Write must succeed (file written) AND clear the stale entry so
	// a later keychain-first Read can't shadow the fresh file.
	_ = fs.Set(testItem, []byte("stale"))
	fs.setErr = errors.New("set boom")
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := Write(testItem, path, []byte("fresh")); err != nil {
		t.Fatalf("Write should not fail on best-effort keychain error: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "fresh" {
		t.Fatalf("file: got %q err=%v", b, err)
	}
	if _, err := fs.Get(testItem); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("stale keychain entry must be cleared, Get err=%v", err)
	}
}

// waired-agent#799. Every sudo-driven init on macOS logged this pair:
//
//	WARN securestore: keychain write failed; file written, clearing any stale entry ...
//	WARN securestore: failed to clear stale keychain entry after write failure
//	     err="... exit status 195 (stderr=\"password has been deleted.\n\")"
//
// Two things were wrong and only one of them was the wording. The first
// line called an expected refusal a failure — there is no session to
// unlock a keychain in when `sudo waired init` hops to its user, so this
// fires on every init of every host, forever. The second attached
// security(1)'s own success chatter to a claim of failure, which reads
// as self-contradiction.
//
// Note what is NOT here: a case treating exit 195 as success. It is
// wrPermErr — the delete really did fail to land — and Read is
// keychain-first, so calling it success would let a leftover item shadow
// the file that was just written.
func TestClassifyWrite(t *testing.T) {
	noSession := fmt.Errorf("keychain set waired/gateway-token: %w", keychain.ErrNoSession)
	denied := fmt.Errorf("keychain delete waired/gateway-token: %w", keychain.ErrDenied)
	boom := errors.New("security: something nobody has a reading for")

	cases := []struct {
		name      string
		setErr    error
		deleteErr error
		wantLevel slog.Level
		wantMsg   string
		wantErr   bool
	}{
		{
			name:      "keychain took the secret",
			wantLevel: slog.LevelDebug,
			wantMsg:   "securestore: secret written to file and keychain",
		},
		{
			name:      "not a keychain platform",
			setErr:    keychain.ErrUnsupported,
			wantLevel: slog.LevelDebug,
			wantMsg:   "securestore: secret written to file (keychain unsupported)",
		},
		{
			// The whole point. One calm line, no error blob, and not
			// a warning: nothing is wrong with this host.
			name:      "no session, and nothing was left behind",
			setErr:    noSession,
			deleteErr: keychain.ErrNotFound,
			wantLevel: slog.LevelInfo,
			wantMsg:   "securestore: keychain not reachable from this session; the protected file holds the secret",
		},
		{
			name:      "no session, and the sweep found nothing to do",
			setErr:    noSession,
			deleteErr: nil,
			wantLevel: slog.LevelInfo,
			wantMsg:   "securestore: keychain not reachable from this session; the protected file holds the secret",
		},
		{
			// This one IS worth a warning, and it is the honest form
			// of the line #799 quotes: an entry that outlived the
			// sweep can be preferred over the file by a later read.
			name:      "no session, and an entry survived the sweep",
			setErr:    noSession,
			deleteErr: denied,
			wantLevel: slog.LevelWarn,
			wantMsg:   "securestore: keychain not reachable from this session, and an older entry could not be cleared; it may shadow the file",
			wantErr:   true,
		},
		{
			name:      "unreadable failure, but the sweep cleaned up",
			setErr:    boom,
			deleteErr: nil,
			wantLevel: slog.LevelWarn,
			wantMsg:   "securestore: keychain write failed; the protected file holds the secret",
			wantErr:   true,
		},
		{
			name:      "unreadable failure, and an entry survived",
			setErr:    boom,
			deleteErr: denied,
			wantLevel: slog.LevelWarn,
			wantMsg:   "securestore: keychain write failed and an older entry could not be cleared; it may shadow the file",
			wantErr:   true,
		},
		{
			// A sweep that reports "unsupported" ran on a platform
			// with no keychain at all; there is nothing to shadow.
			name:      "unreadable failure on a platform with no keychain to sweep",
			setErr:    boom,
			deleteErr: keychain.ErrUnsupported,
			wantLevel: slog.LevelWarn,
			wantMsg:   "securestore: keychain write failed; the protected file holds the secret",
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyWrite(tc.setErr, tc.deleteErr)
			if got.Level != tc.wantLevel {
				t.Errorf("level = %v, want %v", got.Level, tc.wantLevel)
			}
			if got.Msg != tc.wantMsg {
				t.Errorf("msg  = %q\nwant   %q", got.Msg, tc.wantMsg)
			}
			if (got.Err != nil) != tc.wantErr {
				t.Errorf("err  = %v, want an error: %v", got.Err, tc.wantErr)
			}
		})
	}
}

// Read has the same defect Write had and has to be fixed with it:
// a refusal for want of a session is the steady state on the sudo path,
// not a failure, and reporting it as one on every read is the noise
// waired-agent#799 is about.
func TestKeychainMissLevel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want slog.Level
	}{
		{"no session is the steady state, not an event",
			fmt.Errorf("keychain get waired/gateway-token: %w", keychain.ErrNoSession), slog.LevelDebug},
		{"a refused modification is a real failure",
			fmt.Errorf("keychain get waired/gateway-token: %w", keychain.ErrDenied), slog.LevelWarn},
		{"anything unreadable stays loud",
			errors.New("security: exit status 99"), slog.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keychainMissLevel(tc.err); got != tc.want {
				t.Errorf("keychainMissLevel(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// Write must still hold the never-stale invariant when the keychain
// refuses for want of a session — the sweep is what holds it, and
// security(1) will delete from a keychain it will not write to.
func TestWrite_NoSessionStillClearsAStaleEntry(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Set(testItem, []byte("stale"))
	fs.setErr = fmt.Errorf("keychain set: %w", keychain.ErrNoSession)
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := Write(testItem, path, []byte("fresh")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "fresh" {
		t.Fatalf("file: got %q err=%v", b, err)
	}
	if _, err := fs.Get(testItem); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("stale keychain entry must be cleared, Get err=%v", err)
	}
}

func TestRemove_DeletesBoth(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Set(testItem, []byte("payload"))
	use(t, fs)

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(testItem, path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be gone, stat err=%v", err)
	}
	if ok, _ := fs.Exists(testItem); ok {
		t.Fatal("keychain item should be deleted")
	}
}

func TestRemove_MissingFileIsOK(t *testing.T) {
	use(t, newFakeStore())
	if err := Remove(testItem, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("Remove of absent file should be nil, got %v", err)
	}
}

func TestSwapStoreForTest_Restores(t *testing.T) {
	before := currentStore()
	restore := SwapStoreForTest(newFakeStore())
	if currentStore() == before {
		t.Fatal("store was not swapped")
	}
	restore()
	if currentStore() != before {
		t.Fatal("store was not restored")
	}
}
