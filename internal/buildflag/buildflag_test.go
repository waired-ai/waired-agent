package buildflag

import "testing"

// TestInvariants locks the relationship between the per-build constants so a
// future edit to one tag file cannot silently desync the gates from IsProd.
// It compiles under both builds: `go test ./...` exercises the dev arm and
// `go test -tags prod ./internal/buildflag/...` the prod arm.
func TestInvariants(t *testing.T) {
	if IsProd {
		if AllowBypassFlags {
			t.Error("prod build must not allow bypass flags")
		}
		if AllowTestEndpoints {
			t.Error("prod build must not allow test endpoints")
		}
		if DefaultEnv != "prod" {
			t.Errorf("prod build DefaultEnv = %q, want %q", DefaultEnv, "prod")
		}
	} else {
		if !AllowBypassFlags {
			t.Error("dev build must allow bypass flags")
		}
		if !AllowTestEndpoints {
			t.Error("dev build must allow test endpoints")
		}
		if DefaultEnv != "dev" {
			t.Errorf("dev build DefaultEnv = %q, want %q", DefaultEnv, "dev")
		}
	}
}
