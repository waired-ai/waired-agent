package version

import "testing"

func TestAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"0.24.0", "0.6.0", true},
		{"ollama version is 0.24.0", "0.6.0", true},
		{"0.6.0", "0.6.0", true},
		{"0.5.9", "0.6.0", false},
		{"v0.7.1-rc1", "0.6.0", true},
		{"0.6.3.post1", "0.6.0", true},
		{"garbage", "0.6.0", false},
		{"0.6.0", "garbage", false},
		// Historical quirk: shorter v is "older" than a longer min once
		// the shared prefix matches. Preserved for back-compat.
		{"1.2", "1.2.0", false},
		{"1.2.0", "1.2", true},
	}
	for _, c := range cases {
		if got := AtLeast(c.v, c.min); got != c.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b   string
		want   int
		wantOK bool
	}{
		{"1.2.3", "1.2.3", 0, true},
		{"1.2.3", "1.2.4", -1, true},
		{"1.2.4", "1.2.3", 1, true},
		{"0.0.1-rc6", "0.0.1-rc7", 0, true}, // suffix dropped → equal
		{"v1.2.3", "1.2.3", 0, true},        // v-prefix tolerated
		{"1.2", "1.2.0", 0, true},           // zero-padded equality
		{"1.2.0", "1.2", 0, true},
		{"2.0", "1.9.9", 1, true},
		{"0.0.0-dev", "0.0.1", -1, true},
		{"ollama version is 0.6.0", "0.6.0", 0, true}, // last token taken
		{"garbage", "1.0.0", 0, false},
		{"1.0.0", "", 0, false},
	}
	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("Compare(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, got, ok, c.want, c.wantOK)
		}
	}
}
