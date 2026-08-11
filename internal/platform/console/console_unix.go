//go:build !windows

package console

// Linux and macOS terminals decode program output as UTF-8 when the locale
// says so; there is no console code page to set, so both entry points are
// inert and the caller falls through to the locale variables.

// SetOutputUTF8 is a no-op off Windows; the returned restore is a no-op too.
func SetOutputUTF8() (restore func()) { return func() {} }

// OutputIsUTF8 always reports false off Windows — there is no console code
// page, so the caller must decide from the locale instead.
func OutputIsUTF8() bool { return false }
