package controlurl

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#1300): "nothing is configured" and "this
// process cannot see what is configured" are different answers, and only
// the second makes the built-in default a guess.
//
// ParseEnvFile collapses them, deliberately and correctly — for
// RESOLUTION they are the same, because either way this process has no
// URL and the built-in one is what it must use. They part company at the
// moment something PRINTS the result: naming app.waired.ai under a
// sign-in link to a different Control Plane is what an unelevated run did.
func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("a file that names one is readable", func(t *testing.T) {
		p := filepath.Join(dir, "set.env")
		if err := os.WriteFile(p, []byte("WAIRED_CONTROL_URL=https://cp.example\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		url, readable := ReadEnvFile(p)
		if url != "https://cp.example" || !readable {
			t.Errorf("= (%q, %v), want (https://cp.example, true)", url, readable)
		}
	})

	t.Run("a file with no such key is readable and empty", func(t *testing.T) {
		p := filepath.Join(dir, "other.env")
		if err := os.WriteFile(p, []byte("SOMETHING_ELSE=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		url, readable := ReadEnvFile(p)
		if url != "" || !readable {
			t.Errorf("= (%q, %v), want (\"\", true)", url, readable)
		}
	})

	// The ordinary state of a host installed without --control: nothing
	// was configured, so the built-in default is the answer and the
	// sign-in prompt names it as it always has.
	t.Run("an absent file is readable", func(t *testing.T) {
		url, readable := ReadEnvFile(filepath.Join(dir, "nope.env"))
		if url != "" || !readable {
			t.Errorf("= (%q, %v), want (\"\", true)", url, readable)
		}
	})

	// The case the distinction exists for. agent.env is owner-only, so
	// this is what an unelevated `waired init` gets on every host the
	// installer configured.
	t.Run("a file this process cannot open is not readable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("os.Chmod does not remove read access on Windows; the ACL case is exercised on the real host")
		}
		if os.Geteuid() == 0 {
			t.Skip("root opens a 0000 file, so this runner cannot stage the refusal")
		}
		p := filepath.Join(dir, "locked.env")
		if err := os.WriteFile(p, []byte("WAIRED_CONTROL_URL=https://cp.example\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

		url, readable := ReadEnvFile(p)
		if url != "" {
			t.Errorf("url = %q, want empty — the file could not be read", url)
		}
		if readable {
			t.Error("readable = true over a file this process cannot open")
		}
		// ParseEnvFile still answers the same as it always has: the
		// resolution is unchanged, and only the report is new.
		if v := ParseEnvFile(p); v != "" {
			t.Errorf("ParseEnvFile = %q, want \"\" — resolution must not change", v)
		}
	})
}
