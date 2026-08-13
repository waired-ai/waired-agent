package hardware

import (
	"encoding/json"
	"errors"
	"fmt"
)

// appleGPUFallbackModel is what an Apple Silicon GPU is called when
// system_profiler could not name it. The device is still real — every
// Apple Silicon part has an integrated GPU, which is why detectApple
// reports one on architecture alone — so this is a missing NAME, never a
// missing device.
const appleGPUFallbackModel = "Apple GPU"

// appleGPUModel turns one system_profiler run into the model name and,
// when the name could not be read, the warning that says so.
//
// Untagged and pure so all three of its outcomes are table-testable from
// any host: the darwin file holds only the exec. That is the seam rule
// CLAUDE.md §Test discipline asks for, and detectApple had no test of
// any kind before this (waired-agent#35).
//
// The warning is the part that is new. This used to be `if err == nil`
// with no else: a Mac where system_profiler failed, or answered without
// naming a GPU, reported a device called "Apple GPU" and NO error, which
// is the ABSENT/UNKNOWN conflation VendorDetector's contract exists to
// forbid and docs/decisions/20260728/0250-gpu-presence-from-driver-not-path.md
// ruled on for GPU facts generally:
//
//	「不在」と「不明」を別の答えにする … アダプタはあるのに列挙できない
//	場合は必ずエラーを返し、Profile.Errors に出す。
//
// Devices AND an error is the documented shape for exactly this case —
// a real device whose details could not be read — and composeDetectors
// keeps the devices while joining the error into Profile.Errors.
func appleGPUModel(out []byte, probeErr error) (string, error) {
	if probeErr != nil {
		return appleGPUFallbackModel, fmt.Errorf(
			"apple: system_profiler SPDisplaysDataType failed, so this GPU is reported unnamed: %w", probeErr)
	}
	if name := parseSPDisplaysGPUName(out); name != "" {
		return name, nil
	}
	return appleGPUFallbackModel, errors.New(
		"apple: system_profiler SPDisplaysDataType named no GPU, so this GPU is reported unnamed")
}

// parseSPDisplaysGPUName extracts the first GPU model from
// `system_profiler SPDisplaysDataType -json` output. Apple uses
// `sppci_model` (newer macOS) or `_name` (older) as the model key.
func parseSPDisplaysGPUName(out []byte) string {
	var doc struct {
		SPDisplaysDataType []map[string]any `json:"SPDisplaysDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return ""
	}
	for _, d := range doc.SPDisplaysDataType {
		if v, ok := d["sppci_model"].(string); ok && v != "" {
			return v
		}
		if v, ok := d["_name"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
