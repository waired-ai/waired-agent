package runtime

import "strings"

// VLLMVersionOfDir is the vLLM release a version directory under the vLLM
// base holds. A directory is named after the release it was built for, with
// "~N" appended when that name was already taken: every build goes into a
// directory of its own, so no build ever edits or clears one an engine may
// be running from (waired-agent#1431). "~" cannot appear in a PEP 440
// version, so everything before it is the release.
func VLLMVersionOfDir(name string) string {
	v, _, _ := strings.Cut(name, "~")
	return v
}
