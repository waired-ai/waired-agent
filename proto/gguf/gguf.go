// Package gguf reads the header of a GGUF file: its metadata key/values
// and its tensor table. It never reads tensor data.
//
// The header is at the start of the file, so a catalog author can learn
// what a build contains from its first few megabytes — the registry
// serves the blob over HTTP, and a stream that is closed after the tensor
// table costs a 16 MB read instead of a 79 GB pull (waired-agent#1337).
//
// Only what the sizing needs is decoded. Array values are skipped unless
// they are numeric and short: the tokenizer arrays run to hundreds of
// thousands of strings and nothing here reads them.
//
// It lives in the proto module so the control plane can read the header of
// a GGUF a person imports (waired-ai/waired#1476). Like the rest of proto it
// depends on the standard library only. The agent keeps the in-place
// rewrite of a header value in internal/catalog/gguf.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// Value types, as numbered by the GGUF specification.
const (
	typeUint8   = 0
	typeInt8    = 1
	typeUint16  = 2
	typeInt16   = 3
	typeUint32  = 4
	typeInt32   = 5
	typeFloat32 = 6
	typeBool    = 7
	typeString  = 8
	typeArray   = 9
	typeUint64  = 10
	typeInt64   = 11
	typeFloat64 = 12
)

// TypeUint32 is the value type code of a uint32, the only type an in-place
// rewrite of a header value handles.
const TypeUint32 = typeUint32

// maxNumericArray bounds the numeric arrays kept in Header.Arrays. The
// per-layer arrays the sizing reads (head_count_kv) have one element per
// block; anything longer is a tokenizer table.
const maxNumericArray = 4096

// maxKeys and maxTensors bound the two counts at the top of a header. The
// file states them, and the control plane reads the header of a file any
// signed-in person names (waired-ai/waired#1476), so they are input, not
// facts: a header claiming 2^32 tensors must be an error rather than an
// allocation. Published builds carry a few hundred keys and at most a few
// thousand tensors.
const (
	maxKeys    = 1 << 16
	maxTensors = 1 << 16
)

// tensorPrealloc caps the tensor slice reserved up front, so the count a
// header states never decides an allocation on its own.
const tensorPrealloc = 4096

// Tensor is one entry of the tensor table.
type Tensor struct {
	Name  string
	Shape []uint64
	Type  uint32
}

// Elements is the product of the tensor's dimensions.
func (t Tensor) Elements() uint64 {
	n := uint64(1)
	for _, d := range t.Shape {
		n *= d
	}
	return n
}

// Bytes is the tensor's on-disk size, from ggml's block layout for its
// type. It returns an error for a type this package does not know,
// rather than guessing a size.
func (t Tensor) Bytes() (uint64, error) {
	ts, ok := typeSizes[t.Type]
	if !ok {
		return 0, fmt.Errorf("gguf: tensor %s has unknown ggml type %d", t.Name, t.Type)
	}
	return t.Elements() / ts.block * ts.bytes, nil
}

// Header is the decoded part of a GGUF file.
type Header struct {
	Version uint32
	// Scalars holds every scalar metadata value, converted to its Go
	// type: uint64 / int64 / float64 / bool / string.
	Scalars map[string]any
	// Arrays holds numeric metadata arrays of up to maxNumericArray
	// elements, as int64.
	Arrays map[string][]int64
	// ScalarValueAt is the byte offset of each scalar's VALUE within the
	// file, and the GGUF type code stored there. It exists so a value can
	// be rewritten in place without re-serialising the header — see
	// SetArchUint32.
	ScalarValueAt map[string]ValueLocation
	Tensors       []Tensor

	// Complete is true when the whole header was decoded, tensor table
	// included. Read always returns a complete header or an error;
	// ReadPrefix may return one that stops early.
	Complete bool
}

// ValueLocation is where a scalar metadata value sits in the file.
type ValueLocation struct {
	Offset int64
	Type   uint32
}

// Uint returns a scalar integer metadata value and whether it was present.
func (h Header) Uint(key string) (uint64, bool) {
	switch v := h.Scalars[key].(type) {
	case uint64:
		return v, true
	case int64:
		if v >= 0 {
			return uint64(v), true
		}
	}
	return 0, false
}

// Architecture is general.architecture, the prefix of the model's own keys.
func (h Header) Architecture() string {
	s, _ := h.Scalars["general.architecture"].(string)
	return s
}

// ArchUint reads "<architecture>.<suffix>".
func (h Header) ArchUint(suffix string) (uint64, bool) {
	return h.Uint(h.Architecture() + "." + suffix)
}

// TensorBytes sums the sizes of the tensors whose names satisfy match.
func (h Header) TensorBytes(match func(name string) bool) (uint64, error) {
	var sum uint64
	for _, t := range h.Tensors {
		if !match(t.Name) {
			continue
		}
		b, err := t.Bytes()
		if err != nil {
			return 0, err
		}
		sum += b
	}
	return sum, nil
}

// HasTensorPrefix reports whether any tensor name starts with prefix.
func (h Header) HasTensorPrefix(prefix string) bool {
	for _, t := range h.Tensors {
		if strings.HasPrefix(t.Name, prefix) {
			return true
		}
	}
	return false
}

// ErrNotGGUF is returned when the stream does not start with the GGUF magic.
var ErrNotGGUF = errors.New("gguf: not a GGUF file")

// Read decodes the header from r, stopping after the tensor table. It
// reads no further, so r may be an HTTP body the caller closes after.
func Read(r io.Reader) (Header, error) {
	return read(r, false)
}

// ReadPrefix decodes as much of the header as r holds. r is typically the
// first few kilobytes of a GGUF fetched with an HTTP Range request: the
// general.* and architecture keys come first, and the tokenizer arrays that
// follow them run to megabytes. When r ends before the header does,
// ReadPrefix returns every key decoded until then, with Complete false and
// no error. A key cut off part-way is left out. Anything that is not a
// truncation — a wrong magic, an unsupported version, a malformed value —
// is still an error.
func ReadPrefix(r io.Reader) (Header, error) {
	return read(r, true)
}

func read(r io.Reader, prefix bool) (Header, error) {
	h, err := decode(r)
	if err == nil {
		return h, nil
	}
	if prefix && h.Scalars != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		h.Complete = false
		return h, nil
	}
	return Header{}, err
}

// decode is the body of Read. On a read error it still returns the header
// decoded so far, which read keeps only for a prefix.
func decode(r io.Reader) (Header, error) {
	d := decoder{r: bufio.NewReaderSize(r, 1<<20)}
	var magic [4]byte
	if _, err := io.ReadFull(d.r, magic[:]); err != nil {
		return Header{}, fmt.Errorf("gguf: read magic: %w", err)
	}
	if string(magic[:]) != "GGUF" {
		return Header{}, ErrNotGGUF
	}
	// The magic is read straight from the reader rather than through
	// d.read, so start the offset counter past it — every recorded value
	// position is absolute within the file (see Header.ScalarValueAt).
	d.off = int64(len(magic))
	h := Header{Scalars: map[string]any{}, Arrays: map[string][]int64{}, ScalarValueAt: map[string]ValueLocation{}}
	h.Version = d.u32()
	if d.err == nil && h.Version < 2 {
		return Header{}, fmt.Errorf("gguf: version %d is not supported", h.Version)
	}
	nTensors := d.u64()
	nKV := d.u64()
	if d.err != nil {
		return h, d.err
	}
	if nKV > maxKeys {
		return Header{}, fmt.Errorf("gguf: header states %d metadata keys, more than any model has", nKV)
	}
	if nTensors > maxTensors {
		return Header{}, fmt.Errorf("gguf: header states %d tensors, more than any model has", nTensors)
	}
	for i := uint64(0); i < nKV; i++ {
		key := d.str()
		vt := d.u32()
		if d.err != nil {
			return h, d.err
		}
		if vt == typeArray {
			et := d.u32()
			n := d.u64()
			vals, keep := d.array(et, n)
			if d.err != nil {
				return h, fmt.Errorf("gguf: key %s: %w", key, d.err)
			}
			if keep {
				h.Arrays[key] = vals
			}
			continue
		}
		at := d.off
		v := d.scalar(vt)
		if d.err != nil {
			return h, fmt.Errorf("gguf: key %s: %w", key, d.err)
		}
		h.Scalars[key] = v
		h.ScalarValueAt[key] = ValueLocation{Offset: at, Type: vt}
	}
	h.Tensors = make([]Tensor, 0, min(nTensors, tensorPrealloc))
	for i := uint64(0); i < nTensors; i++ {
		name := d.str()
		nDims := d.u32()
		if nDims > 8 {
			return Header{}, fmt.Errorf("gguf: tensor %s has %d dimensions", name, nDims)
		}
		shape := make([]uint64, nDims)
		for j := range shape {
			shape[j] = d.u64()
		}
		typ := d.u32()
		_ = d.u64() // offset into the data section
		if d.err != nil {
			return h, fmt.Errorf("gguf: tensor %d: %w", i, d.err)
		}
		h.Tensors = append(h.Tensors, Tensor{Name: name, Shape: shape, Type: typ})
	}
	h.Complete = true
	return h, nil
}

type decoder struct {
	r   *bufio.Reader
	off int64 // bytes consumed, so a value's position is known
	err error
	buf [8]byte
}

func (d *decoder) read(n int) []byte {
	if d.err != nil {
		return d.buf[:n]
	}
	if _, err := io.ReadFull(d.r, d.buf[:n]); err != nil {
		d.err = err
	}
	d.off += int64(n)
	return d.buf[:n]
}

func (d *decoder) u32() uint32 { return binary.LittleEndian.Uint32(d.read(4)) }
func (d *decoder) u64() uint64 { return binary.LittleEndian.Uint64(d.read(8)) }

func (d *decoder) str() string {
	n := d.u64()
	if d.err != nil {
		return ""
	}
	if n > 1<<24 {
		d.err = fmt.Errorf("string length %d", n)
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(d.r, b); err != nil {
		d.err = err
	}
	d.off += int64(n)
	return string(b)
}

func (d *decoder) skip(n uint64) {
	if d.err != nil {
		return
	}
	if _, err := d.r.Discard(int(n)); err != nil {
		d.err = err
		return
	}
	d.off += int64(n)
}

func (d *decoder) scalar(vt uint32) any {
	switch vt {
	case typeUint8:
		return uint64(d.read(1)[0])
	case typeInt8:
		return int64(int8(d.read(1)[0]))
	case typeUint16:
		return uint64(binary.LittleEndian.Uint16(d.read(2)))
	case typeInt16:
		return int64(int16(binary.LittleEndian.Uint16(d.read(2))))
	case typeUint32:
		return uint64(d.u32())
	case typeInt32:
		return int64(int32(d.u32()))
	case typeFloat32:
		return float64(math.Float32frombits(d.u32()))
	case typeBool:
		return d.read(1)[0] != 0
	case typeString:
		return d.str()
	case typeUint64:
		return d.u64()
	case typeInt64:
		return int64(d.u64())
	case typeFloat64:
		return math.Float64frombits(d.u64())
	}
	d.err = fmt.Errorf("unknown value type %d", vt)
	return nil
}

var scalarWidth = map[uint32]uint64{
	typeUint8: 1, typeInt8: 1, typeBool: 1,
	typeUint16: 2, typeInt16: 2,
	typeUint32: 4, typeInt32: 4, typeFloat32: 4,
	typeUint64: 8, typeInt64: 8, typeFloat64: 8,
}

// array reads n elements of type et. Short integer arrays are returned;
// everything else is skipped.
func (d *decoder) array(et uint32, n uint64) ([]int64, bool) {
	if et == typeString {
		for i := uint64(0); i < n && d.err == nil; i++ {
			l := d.u64()
			d.skip(l)
		}
		return nil, false
	}
	if et == typeArray {
		d.err = errors.New("nested arrays are not supported")
		return nil, false
	}
	w, ok := scalarWidth[et]
	if !ok {
		d.err = fmt.Errorf("unknown array element type %d", et)
		return nil, false
	}
	isInt := et != typeFloat32 && et != typeFloat64 && et != typeBool
	if !isInt || n > maxNumericArray {
		d.skip(w * n)
		return nil, false
	}
	out := make([]int64, n)
	for i := range out {
		switch v := d.scalar(et).(type) {
		case uint64:
			out[i] = int64(v)
		case int64:
			out[i] = v
		}
	}
	return out, true
}
