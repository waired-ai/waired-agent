package catalog

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ollamaSingleRequestFamilies is the list of model families ollama's
// scheduler starts with one slot whatever OLLAMA_NUM_PARALLEL asks, copied
// from ollama v0.34.2 server/sched.go Scheduler.load:
//
//	// Some architectures are not safe with num_parallel > 1.
//	// ref: https://github.com/ollama/ollama/issues/4165
//	if slices.Contains([]string{"mllama", ...}, req.model.Config.ModelFamily) && numParallel != 1 {
//
// A bundled ollama build whose tag names one of these families carries
// max_parallel 1, and no other build does (waired-ai/waired-agent#1423).
// The copy lives in a test on purpose: the product reads the limit from the
// catalog, and a second list in production code would become a second
// source of truth. TestOllamaSingleRequestFamiliesMatchThePin
// (integration) re-reads the pinned sched.go, so a pin bump that changes
// the list fails there.
var ollamaSingleRequestFamilies = []string{
	"mllama", "qwen3vl", "qwen3vlmoe", "qwen35", "qwen35moe", "qwen3next",
	"lfm2", "lfm2moe", "nemotron_h", "nemotron_h_moe", "nemotron_h_omni",
}

// schedSingleRequestListRe matches the slices.Contains call that holds the
// list. The message check below keeps a different Contains over the same
// field from being read as it.
var schedSingleRequestListRe = regexp.MustCompile(`slices\.Contains\(\[\]string\{([^}]*)\},\s*req\.model\.Config\.ModelFamily\)`)

const schedSingleRequestMessage = "does not currently support parallel requests"

// parseSchedSingleRequestFamilies extracts the one-slot family list from
// the source of ollama's server/sched.go. It fails, rather than returning
// an empty list, when the code no longer has the shape it was written
// against: an empty list would read as "ollama lifted every limit".
func parseSchedSingleRequestFamilies(src string) ([]string, error) {
	matches := schedSingleRequestListRe.FindAllStringSubmatch(src, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("found %d slices.Contains lists over req.model.Config.ModelFamily, want 1; re-read Scheduler.load", len(matches))
	}
	if n := strings.Count(src, schedSingleRequestMessage); n != 1 {
		return nil, fmt.Errorf("found %q %d times, want 1; re-read Scheduler.load", schedSingleRequestMessage, n)
	}
	var out []string
	for _, part := range strings.Split(matches[0][1], ",") {
		name := strings.Trim(strings.TrimSpace(part), `"`)
		if name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the family list is empty; re-read Scheduler.load")
	}
	return out, nil
}

func TestParseSchedSingleRequestFamilies(t *testing.T) {
	const pinned = `
	// Some architectures are not safe with num_parallel > 1.
	// ref: https://github.com/ollama/ollama/issues/4165
	if slices.Contains([]string{"mllama", "qwen3vl", "qwen3vlmoe", "qwen35", "qwen35moe", "qwen3next", "lfm2", "lfm2moe", "nemotron_h", "nemotron_h_moe", "nemotron_h_omni"}, req.model.Config.ModelFamily) && numParallel != 1 {
		numParallel = 1
		slog.Warn("model architecture does not currently support parallel requests", "architecture", req.model.Config.ModelFamily)
	}
`
	got, err := parseSchedSingleRequestFamilies(pinned)
	if err != nil {
		t.Fatalf("the v0.34.2 shape: %v", err)
	}
	if !slices.Equal(got, ollamaSingleRequestFamilies) {
		t.Errorf("parsed %v, want the copied list %v", got, ollamaSingleRequestFamilies)
	}

	for name, src := range map[string]string{
		"the check removed": `numParallel := max(int(envconfig.NumParallel()), 1)`,
		"the list moved to a variable": strings.Replace(pinned,
			`[]string{"mllama", "qwen3vl", "qwen3vlmoe", "qwen35", "qwen35moe", "qwen3next", "lfm2", "lfm2moe", "nemotron_h", "nemotron_h_moe", "nemotron_h_omni"}`,
			`singleSlotFamilies`, 1),
		"the message reworded": strings.Replace(pinned, schedSingleRequestMessage, "is limited to one request", 1),
		"the list emptied":     strings.Replace(pinned, `"mllama", "qwen3vl", "qwen3vlmoe", "qwen35", "qwen35moe", "qwen3next", "lfm2", "lfm2moe", "nemotron_h", "nemotron_h_moe", "nemotron_h_omni"`, ``, 1),
	} {
		if got, err := parseSchedSingleRequestFamilies(src); err == nil {
			t.Errorf("%s: parsed %v, want an error that sends a reader back to sched.go", name, got)
		}
	}
}
