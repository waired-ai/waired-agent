package mempressure

import "testing"

// TestParsePressure works on the text real kernels produced. The first two
// bodies were captured on 2026-09-20 from a host at rest and from a cgroup
// 1.3 seconds into a deliberate thrash.
func TestParsePressure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantSome float64
		wantFull float64
		wantErr  bool
	}{
		{
			name:     "a host at rest",
			body:     "some avg10=0.00 avg60=0.00 avg300=0.00 total=9063226\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=9030535\n",
			wantSome: 0, wantFull: 0,
		},
		{
			name:     "1.3s into a thrash",
			body:     "some avg10=8.51 avg60=1.42 avg300=0.28 total=46475630\nfull avg10=8.51 avg60=1.42 avg300=0.28 total=42438609\n",
			wantSome: 8.51, wantFull: 8.51,
		},
		{
			name:     "cgroup v2 root, which omits the full line",
			body:     "some avg10=1.25 avg60=0.40 avg300=0.10 total=123\n",
			wantSome: 1.25, wantFull: 0,
		},
		{
			name:    "empty",
			body:    "",
			wantErr: true,
		},
		{
			name:    "a header this parser does not understand",
			body:    "some total=123\n",
			wantErr: true,
		},
		{
			name:    "avg10 that is not a number",
			body:    "some avg10=abc avg60=0.00\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			some, full, err := parsePressure([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("parsePressure err = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if some != tc.wantSome || full != tc.wantFull {
				t.Errorf("parsePressure = some %v full %v, want some %v full %v",
					some, full, tc.wantSome, tc.wantFull)
			}
		})
	}
}
