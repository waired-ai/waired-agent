package detach

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestStartRefusesInheritedStreams pins the one mistake this package exists to
// prevent. A nil stream is inherited, so a child started from a SessionStart
// hook would hold the write end of the hook's stdout pipe; Claude Code waits
// for that pipe to close before the session begins, and a child that sleeps
// 20 s would stall every launch. Refusing beats redirecting silently: a caller
// whose output disappeared without being asked would have no way to notice.
func TestStartRefusesInheritedStreams(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()

	for _, tc := range []struct {
		name                  string
		stdin, stdout, stderr *os.File
	}{
		{"stdin inherited", nil, null, null},
		{"stdout inherited", null, nil, null},
		{"stderr inherited", null, null, nil},
		{"all inherited", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestStartRefusesInheritedStreams/nothing")
			// A nil *os.File in an io.Writer field is not a nil interface,
			// so assign only when the case wants the stream set.
			if tc.stdin != nil {
				cmd.Stdin = tc.stdin
			}
			if tc.stdout != nil {
				cmd.Stdout = tc.stdout
			}
			if tc.stderr != nil {
				cmd.Stderr = tc.stderr
			}
			err := Start(cmd)
			if err == nil {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				t.Fatal("Start accepted an inherited stream")
			}
			if !strings.Contains(err.Error(), "standard streams") {
				t.Errorf("error does not say what is wrong: %v", err)
			}
		})
	}
}
