package console

import (
	"errors"
	"testing"
)

// TestCPPlan pins the decision that #629's fix turns on. Product contract:
// waired-ai/waired-agent#629 asks for output that "survives on a console whose
// code page is not UTF-8"; the choice to do that by setting the code page (the
// same thing install.ps1 has always done) was ratified by the owner on
// waired-ai/waired#1127 L37.
//
// Untagged so it runs on the Linux CI job too — the Windows-only arm of this
// package would otherwise be exercised nowhere on a PR.
func TestCPPlan(t *testing.T) {
	errNoConsole := errors.New("The handle is invalid.")

	tests := []struct {
		name       string
		cur        uint32
		err        error
		wantPrior  uint32
		wantChange bool
	}{
		{
			// waired-agent under the SCM, waired-tray, and any process
			// started without a console: nothing to set, nothing to restore.
			name: "no console at all", cur: 0, err: errNoConsole,
			wantPrior: 0, wantChange: false,
		},
		{
			// A console already on CP_UTF8 (Windows Terminal with the
			// "use Unicode UTF-8" option, or install.ps1 left it there).
			// Setting it again would manufacture a restore we do not owe.
			name: "already CP_UTF8", cur: CPUTF8, err: nil,
			wantPrior: CPUTF8, wantChange: false,
		},
		{
			// The reported host: Japanese Windows 11, chcp 932.
			name: "japanese CP932", cur: 932, err: nil,
			wantPrior: 932, wantChange: true,
		},
		{
			// US/Western default, mojibake for the same reason CP932 is.
			name: "western CP1252", cur: 1252, err: nil,
			wantPrior: 1252, wantChange: true,
		},
		{
			// The OEM page a plain cmd.exe starts on.
			name: "OEM CP437", cur: 437, err: nil,
			wantPrior: 437, wantChange: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prior, change := cpPlan(tt.cur, tt.err)
			if change != tt.wantChange {
				t.Errorf("change = %v, want %v", change, tt.wantChange)
			}
			if prior != tt.wantPrior {
				t.Errorf("prior = %d, want %d", prior, tt.wantPrior)
			}
		})
	}
}

// TestCPPlanRestoresTheOriginalPage states the consequence the plan exists for:
// whenever the caller changes the page it is handed the page to put back, so a
// console is never left on CP_UTF8 after waired exits. The code page belongs to
// the console window, not to this process.
func TestCPPlanRestoresTheOriginalPage(t *testing.T) {
	for _, cp := range []uint32{932, 437, 1252, 65000} {
		prior, change := cpPlan(cp, nil)
		if !change {
			t.Fatalf("cp %d: change = false, want true", cp)
		}
		if prior != cp {
			t.Errorf("cp %d: prior = %d, want the page we found", cp, prior)
		}
	}
}
