package keychain

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// Every stderr below is verbatim security(1) output, not a plausible
// imitation of one. A fake that accepts invented text cannot fail on a
// grammar the real tool does not speak.
//
// Sources:
//   - 36 (add), 44, 45 and the empty-stderr 36: captured 2026-08-21 on
//     macOS 26.6.2 against a throwaway keychain in /tmp, created and
//     deleted by the probe.
//   - 195: captured in the rc9 3-OS verification run and quoted verbatim
//     in waired-agent#799.
//
// This is a record of today's behaviour, not a product contract: the
// codes are Apple's, and security(1) may add paths this build has no
// reading for — which is what outcomeOther is for.
func TestClassifySecurity(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		stderr   string
		want     outcome
	}{
		{
			name:     "success",
			exitCode: 0,
			want:     outcomeOK,
		},
		{
			// The status is the only witness here. security(1) said
			// nothing at all, so a classifier reading stderr alone
			// sees a silent success where the read was refused.
			name:     "locked keychain refuses a read and says nothing",
			exitCode: 36,
			stderr:   "",
			want:     outcomeNoSession,
		},
		{
			name:     "locked keychain refuses a write",
			exitCode: 36,
			stderr:   "security: SecKeychainItemCreateFromContent (/tmp/probe.keychain): User interaction is not allowed.\n",
			want:     outcomeNoSession,
		},
		{
			// Two error sentences, one verdict. Only the status says
			// which of them is the reason the write did not land.
			name:     "no session, reported behind a write-permission line",
			exitCode: 36,
			stderr: "security: SecKeychainItemModifyContent: Write permissions error.\n" +
				"security: SecKeychainItemCreateFromContent (<default>): User interaction is not allowed.\n",
			want: outcomeNoSession,
		},
		{
			name:     "item not found",
			exitCode: 44,
			stderr:   "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n",
			want:     outcomeNotFound,
		},
		{
			name:     "item already exists",
			exitCode: 45,
			stderr:   "security: SecKeychainItemCreateFromContent (/Library/Keychains/System.keychain): The specified item already exists in the keychain.\n",
			want:     outcomeDuplicate,
		},
		{
			// waired-agent#799's second WARN. stderr is security(1)'s
			// own success chatter and carries no error text, so
			// nothing but the status distinguishes this from a clean
			// delete — which is exactly how it came to be logged as a
			// failure whose evidence read like a success.
			name:     "delete removed one item and could not write another",
			exitCode: 195,
			stderr:   "password has been deleted.\n",
			want:     outcomeDenied,
		},
		{
			name:     "a status this build has no reading for",
			exitCode: 99,
			stderr:   "totally unexpected failure\n",
			want:     outcomeOther,
		},
		{
			// The process never ran, so there is no status. Falls
			// through to the prose backstop, which has nothing to go
			// on either.
			name:     "command could not be started",
			exitCode: -1,
			stderr:   "",
			want:     outcomeOther,
		},
		{
			name:     "no status, but the prose names the item as missing",
			exitCode: -1,
			stderr:   "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n",
			want:     outcomeNotFound,
		},
		{
			name:     "no status, but the prose names the session",
			exitCode: -1,
			stderr:   "security: SecKeychainItemCreateFromContent (<default>): User interaction is not allowed.\n",
			want:     outcomeNoSession,
		},
		{
			name:     "no status, but the prose names the duplicate",
			exitCode: -1,
			stderr:   "security: SecKeychainItemCreateFromContent (<default>): The specified item already exists in the keychain.\n",
			want:     outcomeDuplicate,
		},
		{
			// The numeric form, for the paths where security(1) prints
			// the OSStatus instead of the sentence.
			name:     "no status, numeric errSecItemNotFound",
			exitCode: -1,
			stderr:   "security: returned -25300\n",
			want:     outcomeNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySecurity(tc.exitCode, []byte(tc.stderr)); got != tc.want {
				t.Fatalf("classifySecurity(%d, %q) = %v, want %v",
					tc.exitCode, tc.stderr, got, tc.want)
			}
		})
	}
}

// exitCodeOf is the half of runSecurityReal that can be tested off
// darwin, and it has to distinguish "ran and refused" from "never ran":
// only the first has a status to classify.
func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != 0 {
		t.Fatalf("exitCodeOf(nil) = %d, want 0", got)
	}
	if got := exitCodeOf(errors.New("could not start")); got != -1 {
		t.Fatalf("exitCodeOf(plain error) = %d, want -1", got)
	}

	// A real *exec.ExitError, produced by a real process, so this
	// asserts against the type runSecurityReal actually receives.
	err := exec.Command("sh", "-c", "exit 44").Run()
	if got := exitCodeOf(err); got != 44 {
		t.Fatalf("exitCodeOf(exit 44) = %d, want 44", got)
	}

	// Wrapped, because callers up the stack add context.
	if got := exitCodeOf(fmt.Errorf("keychain delete waired/gateway-token: %w", err)); got != 44 {
		t.Fatalf("exitCodeOf(wrapped exit 44) = %d, want 44", got)
	}
}
