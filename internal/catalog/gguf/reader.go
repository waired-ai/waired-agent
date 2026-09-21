package gguf

import (
	"io"

	pgguf "github.com/waired-ai/waired-agent/proto/gguf"
)

// The header reader lives in proto/gguf so that the control plane can
// read the first few kilobytes of a GGUF a person imports and estimate
// its KV cache (waired-ai/waired#1476). This package keeps the in-place
// rewrite (patch.go), which writes files and so stays in the agent, and
// re-exports the reader so its callers read one package.

type (
	Header        = pgguf.Header
	Tensor        = pgguf.Tensor
	ValueLocation = pgguf.ValueLocation
)

// ErrNotGGUF is returned when the stream does not start with the GGUF magic.
var ErrNotGGUF = pgguf.ErrNotGGUF

// Read decodes the header from r, stopping after the tensor table.
func Read(r io.Reader) (Header, error) { return pgguf.Read(r) }

// TypeName is the ggml name of a tensor type, for reports.
func TypeName(t uint32) string { return pgguf.TypeName(t) }

// typeUint32 is the GGUF value type code of a uint32, as numbered by the
// specification; patch.go rewrites only values of this type.
const typeUint32 = pgguf.TypeUint32
