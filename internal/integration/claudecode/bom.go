package claudecode

// utf8BOM is the byte-order mark Windows editors and PowerShell put at the head
// of a UTF-8 file: `Set-Content -Encoding utf8` (5.1), `>` redirection, and
// Notepad's "UTF-8 with BOM" all produce one. encoding/json rejects it, and
// Claude Code does not — so a file with one is a file waired alone cannot read
// (waired-agent#1067).
//
// The same allowance lives in internal/integration/claudemanaged and, since
// #1002, in internal/integration/openclaw. Three copies of three bytes, in
// three packages that must not depend on each other.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}
