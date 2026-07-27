package main

import "os/exec"

// Test files are out of scope: a probe in a test decides nothing on a
// user's machine.
func inTest() { _, _ = exec.LookPath("kubectl") }
