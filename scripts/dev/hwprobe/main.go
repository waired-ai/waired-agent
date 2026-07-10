// Command hwprobe runs the agent's hardware profiler once and prints
// the JSON result. Useful for verifying GPU / RAM / CPU detection on a
// new host (especially Windows) without needing a full waired-agent
// identity / enrollment.
//
// Build: `go run ./scripts/dev/hwprobe`
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

func main() {
	cachePath := "."
	if len(os.Args) > 1 {
		cachePath = os.Args[1]
	}
	p := hardware.NewProfiler(cachePath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prof := p.Profile(ctx)
	out, err := json.MarshalIndent(prof, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
