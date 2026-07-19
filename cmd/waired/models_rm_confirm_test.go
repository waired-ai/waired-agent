package main

import (
	"strings"
	"testing"
)

// TestModelsRm_NonTTYRequiresYes pins the waired#845 §8.2 guard: model
// removal prompts for confirmation, and a non-TTY caller (this test)
// must pass --yes explicitly — otherwise the command aborts before any
// management-API call is attempted.
func TestModelsRm_NonTTYRequiresYes(t *testing.T) {
	cmd := newModelsRmCmd()
	cmd.SetArgs([]string{"some-model"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Execute() = %v, want aborted error without --yes on non-TTY stdin", err)
	}
}
