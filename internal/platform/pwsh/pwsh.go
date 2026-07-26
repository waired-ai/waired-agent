// Package pwsh builds the environment for spawning Windows PowerShell 5.1
// (`powershell.exe`) as a child process.
//
// Windows PowerShell 5.1 and PowerShell 7 ship separate, incompatible copies
// of the same in-box modules. PowerShell 7 puts its own module root first in
// PSModulePath, and a 5.1 child that inherits that value tries to autoload
// pwsh 7's Microsoft.PowerShell.Security — whose Security.types.ps1xml
// collides with 5.1's built-in types file:
//
//	FormatXmlUpdateException: ... "AuditToString" is already present
//
// Autoloading then fails for the whole module, so cmdlets that live in it
// (notably Get-AuthenticodeSignature) can never load. On the supported
// Windows install path `waired init` runs under an elevated PowerShell 7,
// so every 5.1 child we spawn inherited exactly that broken value: the
// Ollama installer's signature check became a terminating error and the
// engine install failed on every attempt with `exit status 1` (#178).
//
// Dropping the variable entirely is the fix rather than rewriting it:
// with PSModulePath absent from the child's environment, PowerShell 5.1
// rebuilds the correct path itself from $PSHOME and the registry. Note
// that an explicit `Import-Module Microsoft.PowerShell.Security` is NOT a
// workaround — the explicit import fails the same way.
//
// The package is deliberately untagged so the (pure) filtering is compiled
// and unit-tested on every OS; on Unix the variable is simply absent and
// ChildEnv is an identity transform.
package pwsh

import (
	"os"
	"strings"
)

// psModulePathKey is the environment variable to strip, upper-cased.
// Windows environment names are case-insensitive and PowerShell spells it
// "PSModulePath", so callers must compare case-insensitively.
const psModulePathKey = "PSMODULEPATH"

// ChildEnv returns env with any PSModulePath entry removed. It never
// mutates the input slice.
func ChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, psModulePathKey) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Env is the current process environment made safe to hand to Windows
// PowerShell 5.1. Assign it to exec.Cmd.Env at every `powershell` spawn:
// leaving Cmd.Env nil inherits the parent environment verbatim, which is
// the bug this package exists to prevent.
func Env() []string { return ChildEnv(os.Environ()) }
