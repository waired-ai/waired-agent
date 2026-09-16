package runtime

import (
	"os"
	"testing"
)

// TestMain seals machine-global state the package reads.
//
// UV_CACHE_DIR: the vLLM installer points uv's cache under the state dir
// unless the caller already exported one (waired-ai/waired#1435). A
// developer machine or a CI runner that exports it (the GPU runner image
// does) would otherwise flip every installer test onto the inherited
// branch. Tests of that branch set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("UV_CACHE_DIR")
	os.Exit(m.Run())
}
