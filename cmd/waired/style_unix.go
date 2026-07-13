//go:build linux || darwin

package main

// enableVTProcessing is a no-op off Windows: Linux and macOS terminals render
// ANSI SGR sequences natively.
func enableVTProcessing() {}
