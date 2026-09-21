//go:build windows

package runtime

import "context"

// Lock holds nothing on Windows. The overlap it prevents on the Unixes —
// the daemon's start-up converge and the installer's running at once
// (waired-agent#1511) — does not arise here: install.ps1 stops the service
// before it converges the engine and starts it after, so the daemon's own
// converge finds the engine at the pin. A hand-run `waired runtimes upgrade
// ollama` while the service is starting is the one overlap left.
func (i *OllamaInstaller) Lock(context.Context, func()) (func(), error) {
	return func() {}, nil
}
