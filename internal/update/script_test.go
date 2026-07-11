package update

import (
	"slices"
	"testing"
)

func TestScriptName(t *testing.T) {
	if got := ScriptName("windows"); got != "install.ps1" {
		t.Errorf("windows = %q, want install.ps1", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := ScriptName(goos); got != "install.sh" {
			t.Errorf("%s = %q, want install.sh", goos, got)
		}
	}
}

func TestScriptBaseURL(t *testing.T) {
	t.Setenv("WAIRED_INSTALL_BASE_URL", "")
	if got := ScriptBaseURL(); got != "https://github.com/waired-ai/waired-agent/releases/latest/download" {
		t.Errorf("default base = %q", got)
	}
	t.Setenv("WAIRED_INSTALL_BASE_URL", "https://example.test/dl")
	if got := ScriptBaseURL(); got != "https://example.test/dl" {
		t.Errorf("override base = %q", got)
	}
}

func TestScriptBaseURLForChannel(t *testing.T) {
	const (
		stable = "https://github.com/waired-ai/waired-agent/releases/latest/download"
		edge   = "https://github.com/waired-ai/waired-agent/releases/download/edge"
	)
	t.Setenv("WAIRED_INSTALL_BASE_URL", "")
	if got := ScriptBaseURLForChannel("stable"); got != stable {
		t.Errorf("stable base = %q, want %q", got, stable)
	}
	if got := ScriptBaseURLForChannel(""); got != stable {
		t.Errorf("empty base = %q, want stable %q", got, stable)
	}
	if got := ScriptBaseURLForChannel("edge"); got != edge {
		t.Errorf("edge base = %q, want %q", got, edge)
	}
	// The env override wins for every channel (mirror / pin escape hatch).
	t.Setenv("WAIRED_INSTALL_BASE_URL", "https://example.test/dl")
	for _, ch := range []string{"", "stable", "edge"} {
		if got := ScriptBaseURLForChannel(ch); got != "https://example.test/dl" {
			t.Errorf("override base for %q = %q", ch, got)
		}
	}
}

func TestScriptURLForChannel(t *testing.T) {
	t.Setenv("WAIRED_INSTALL_BASE_URL", "")
	if got := ScriptURLForChannel("linux", "edge"); got != "https://github.com/waired-ai/waired-agent/releases/download/edge/install.sh" {
		t.Errorf("linux edge = %q", got)
	}
	if got := ScriptURLForChannel("windows", "stable"); got != "https://github.com/waired-ai/waired-agent/releases/latest/download/install.ps1" {
		t.Errorf("windows stable = %q", got)
	}
}

func TestInstallerArgs(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		path      string
		checkOnly bool
		yes       bool
		channel   string
		wantCmd   string
		wantArgs  []string
	}{
		{"linux apply yes no-channel", "linux", "/tmp/i.sh", false, true, "", "sh", []string{"/tmp/i.sh", "--update", "--yes"}},
		{"linux apply no-yes no-channel", "linux", "/tmp/i.sh", false, false, "", "sh", []string{"/tmp/i.sh", "--update"}},
		{"linux apply edge", "linux", "/tmp/i.sh", false, false, "edge", "sh", []string{"/tmp/i.sh", "--update", "--edge"}},
		{"linux apply stable yes", "linux", "/tmp/i.sh", false, true, "stable", "sh", []string{"/tmp/i.sh", "--update", "--yes", "--stable"}},
		{"linux check no-channel", "linux", "/tmp/i.sh", true, false, "", "sh", []string{"/tmp/i.sh", "--check"}},
		{"linux check edge", "linux", "/tmp/i.sh", true, false, "edge", "sh", []string{"/tmp/i.sh", "--check", "--edge"}},
		{"darwin apply edge yes", "darwin", "/tmp/i.sh", false, true, "edge", "sh", []string{"/tmp/i.sh", "--update", "--yes", "--edge"}},
		{"windows apply yes no-channel", "windows", `C:\i.ps1`, false, true, "", "powershell", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", `C:\i.ps1`, "-Update", "-Yes"}},
		{"windows apply stable", "windows", `C:\i.ps1`, false, false, "stable", "powershell", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", `C:\i.ps1`, "-Update", "-Stable"}},
		{"windows apply edge yes", "windows", `C:\i.ps1`, false, true, "edge", "powershell", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", `C:\i.ps1`, "-Update", "-Yes", "-Edge"}},
		{"windows check edge", "windows", `C:\i.ps1`, true, false, "edge", "powershell", []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", `C:\i.ps1`, "-Check", "-Edge"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, args := InstallerArgs(tt.goos, tt.path, tt.checkOnly, tt.yes, tt.channel)
			if cmd != tt.wantCmd {
				t.Errorf("cmd = %q, want %q", cmd, tt.wantCmd)
			}
			if !slices.Equal(args, tt.wantArgs) {
				t.Errorf("args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}
