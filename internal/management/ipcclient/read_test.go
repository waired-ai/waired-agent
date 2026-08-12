package ipcclient

import "testing"

// TestSameAuthority pins the rule that decides whether a read may go over
// the socket. Both spellings of the CLI's own default must compare equal to
// each other, and anything the operator changed must not.
func TestSameAuthority(t *testing.T) {
	const def = "http://127.0.0.1:9476"

	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", def, def, true},
		{"schemeless-vs-scheme", "127.0.0.1:9476", def, true},
		{"trailing-slash", "http://127.0.0.1:9476/", def, true},
		{"whitespace", "  http://127.0.0.1:9476  ", def, true},
		{"uppercase-host", "http://127.0.0.1:9476", "HTTP://127.0.0.1:9476", true},
		{"other-port", "http://127.0.0.1:9999", def, false},
		{"other-host", "http://192.168.1.5:9476", def, false},
		// localhost is not folded into 127.0.0.1: a base URL the operator
		// spelled differently is treated as a different daemon, which is
		// the conservative direction (TCP, no socket redirect).
		{"localhost-not-folded", "http://localhost:9476", def, false},
		{"empty-a", "", def, false},
		{"empty-b", def, "", false},
		{"both-empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameAuthority(tc.a, tc.b); got != tc.want {
				t.Fatalf("SameAuthority(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
