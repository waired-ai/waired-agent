package runtime

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// The engine's own account of the GPUs it will use (waired-agent#1513).
//
// ollama logs it once per `ollama serve`, before it answers any request
// (ollama v0.34.2 server/routes.go Serve): "Listening on …", then one
// `msg="inference compute"` line per device it kept (discover/types.go
// LogDetails) — or a single `library=cpu` line when it kept none — then
// `msg="vram-based default context"`. Integrated GPUs it set aside are each
// logged as `msg="dropping integrated GPU; …"` (discover/runner.go). By the
// time the readiness poll passes, the block is in engine.log, which keeps
// its start when it reaches its cap. The output reaches the file through a
// pipe, so the block can still be a line short when it is read: without the
// closing line it is reported as incomplete rather than guessed at.
//
// AT EVERY PIN MOVE: re-read those three sites. A changed message or field
// name turns this into "incomplete", never into a wrong answer.

// EngineDevice is one device the engine reported using.
type EngineDevice struct {
	Library     string // CUDA, ROCm, Vulkan, Metal
	Compute     string // "12.0" on CUDA, "gfx1151" on ROCm, "0.0" where the backend has none
	Name        string
	Description string
	Type        string // "discrete" or "iGPU"
	Total       string
}

// EngineDiscovery is what one engine start reported.
type EngineDiscovery struct {
	Devices []EngineDevice
	// CPUOnly is the engine saying it kept no GPU at all.
	CPUOnly bool
	// Dropped is the integrated GPUs it set aside.
	Dropped []EngineDevice
}

// ParseInferenceCompute reads the device block of the last engine start in
// an engine.log head. ok is false when there is no complete block to read.
func ParseInferenceCompute(head string) (EngineDiscovery, bool) {
	head = strings.ReplaceAll(head, "\r\n", "\n")
	if i := strings.LastIndex(head, `msg="Listening on `); i >= 0 {
		head = head[i:]
	} else {
		return EngineDiscovery{}, false
	}
	var out EngineDiscovery
	complete := false
	for _, line := range strings.Split(head, "\n") {
		kv := parseLogfmt(line)
		switch kv["msg"] {
		case "inference compute":
			d := engineDeviceFrom(kv)
			if strings.EqualFold(d.Library, "cpu") {
				out.CPUOnly = true
				continue
			}
			out.Devices = append(out.Devices, d)
		case "dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1":
			out.Dropped = append(out.Dropped, engineDeviceFrom(kv))
		case "vram-based default context":
			complete = true
		}
		if complete {
			break
		}
	}
	if !complete || (len(out.Devices) == 0 && !out.CPUOnly) {
		return EngineDiscovery{}, false
	}
	return out, true
}

func engineDeviceFrom(kv map[string]string) EngineDevice {
	return EngineDevice{
		Library: kv["library"], Compute: kv["compute"], Name: kv["name"],
		Description: kv["description"], Type: kv["type"], Total: kv["total"],
	}
}

// parseLogfmt splits one slog text line into its key=value pairs. Values
// slog had to quote are Go-quoted strings, so strconv reads them back
// exactly, escaped quotes included.
func parseLogfmt(line string) map[string]string {
	kv := map[string]string{}
	for line = strings.TrimSpace(line); line != ""; line = strings.TrimLeft(line, " ") {
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			break
		}
		key := line[:eq]
		if strings.ContainsAny(key, " \"") {
			break
		}
		rest := line[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				break
			}
			val, _ = strconv.Unquote(q)
			rest = rest[len(q):]
		} else {
			end := strings.IndexByte(rest, ' ')
			if end < 0 {
				end = len(rest)
			}
			val, rest = rest[:end], rest[end:]
		}
		kv[key] = val
		line = rest
	}
	return kv
}

// engineDiscoveryHeadBytes is how much of engine.log's start is read for
// the device block: far more than the block, far less than the log.
const engineDiscoveryHeadBytes = 256 << 10

// EngineLogHead returns the start of the current engine.log, up to maxBytes.
// engine.log is rotated per spawn and keeps its start when it reaches its
// cap, so the head is the current engine's own start-up. "" when there is
// no log.
func (a *OllamaAdapter) EngineLogHead(maxBytes int) string {
	return headEngineLog(a.engineLogPath(), maxBytes)
}

func headEngineLog(path string, maxBytes int) string {
	if path == "" || maxBytes <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	if err != nil {
		return ""
	}
	return string(b)
}

// EngineDiscoveryHead is EngineLogHead at the size the device block needs.
func (a *OllamaAdapter) EngineDiscoveryHead() string {
	return a.EngineLogHead(engineDiscoveryHeadBytes)
}
