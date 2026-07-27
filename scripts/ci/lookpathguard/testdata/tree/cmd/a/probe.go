package main

import "os/exec"

// A literal argument is recorded by the binary it names.
func haveSudo() bool {
	_, err := exec.LookPath("sudo")
	return err == nil
}

// A non-literal argument is recorded by its source expression, so the
// table entry is something a reviewer can grep for.
func haveEngine(binary string) bool {
	_, err := exec.LookPath(binary)
	return err == nil
}

// Not exec.LookPath: a same-named method on something else must not be
// collected, or the table fills with noise nobody can act on.
type finder struct{}

func (finder) LookPath(string) (string, error) { return "", nil }

func viaOther(f finder) { _, _ = f.LookPath("sudo") }
