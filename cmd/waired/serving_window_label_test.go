package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// `waired status` printed the served window as tokens/1024, which named
// neither of the two windows an engine actually serves: the coding window
// came out as "196k" and the long one would have come out as "1024k". The
// product calls them 200k and 1M everywhere else.
func TestServingWindowLabel(t *testing.T) {
	for _, tc := range []struct {
		tokens int
		want   string
	}{
		{hostfit.ServingWindow200k, "200k"},
		{hostfit.ServingWindow1M, "1M"},
		// Neither rung: printed as it is. A window between or below the two
		// means an engine is not serving what it was asked for, and rounding
		// it into "196k" would hide exactly that.
		{262144, "262144"},
		{32768, "32768"},
		{0, "0"},
	} {
		if got := servingWindowLabel(tc.tokens); got != tc.want {
			t.Errorf("servingWindowLabel(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
	}
}
