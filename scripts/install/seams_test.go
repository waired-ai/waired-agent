package installscripts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
)

// TestMain seals the Claude Code managed-settings path for the whole test
// binary. It is machine-global (/etc/claude-code,
// /Library/Application Support/ClaudeCode, %ProgramFiles%\ClaudeCode), and the
// leftovers corpus test drives the real removers against it; a case that forgot
// its own swap would otherwise edit the managed settings of the computer
// running the tests (#386's shape, as claudemanaged.SwapPathForTest records).
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "waired-installscripts-test")
	if err != nil {
		panic("seal managed settings: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(dir) }()
	restore := claudemanaged.SwapPathForTest(filepath.Join(dir, "managed-settings.json"))
	defer restore()
	return m.Run()
}
