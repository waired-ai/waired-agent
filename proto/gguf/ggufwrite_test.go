package gguf

import (
	"bytes"
	"encoding/binary"
	"math"
)

// headerBuilder writes a GGUF v3 header the way llama.cpp's writer lays
// it out, for tests: magic, version, counts, key/values, tensor table. It
// writes no tensor data — the reader must not need any.
type headerBuilder struct {
	kvs     bytes.Buffer
	tensors bytes.Buffer
	nKV     uint64
	nTensor uint64
}

func (b *headerBuilder) str(w *bytes.Buffer, s string) {
	_ = binary.Write(w, binary.LittleEndian, uint64(len(s)))
	w.WriteString(s)
}

func (b *headerBuilder) u32(key string, v uint32) {
	b.str(&b.kvs, key)
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeUint32))
	_ = binary.Write(&b.kvs, binary.LittleEndian, v)
	b.nKV++
}

func (b *headerBuilder) f32(key string, v float32) {
	b.str(&b.kvs, key)
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeFloat32))
	_ = binary.Write(&b.kvs, binary.LittleEndian, math.Float32bits(v))
	b.nKV++
}

func (b *headerBuilder) text(key, v string) {
	b.str(&b.kvs, key)
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeString))
	b.str(&b.kvs, v)
	b.nKV++
}

func (b *headerBuilder) i32s(key string, vs []int32) {
	b.str(&b.kvs, key)
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeArray))
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeInt32))
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint64(len(vs)))
	for _, v := range vs {
		_ = binary.Write(&b.kvs, binary.LittleEndian, v)
	}
	b.nKV++
}

func (b *headerBuilder) strs(key string, vs []string) {
	b.str(&b.kvs, key)
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeArray))
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint32(typeString))
	_ = binary.Write(&b.kvs, binary.LittleEndian, uint64(len(vs)))
	for _, v := range vs {
		b.str(&b.kvs, v)
	}
	b.nKV++
}

func (b *headerBuilder) tensor(name string, typ uint32, shape ...uint64) {
	b.str(&b.tensors, name)
	_ = binary.Write(&b.tensors, binary.LittleEndian, uint32(len(shape)))
	for _, d := range shape {
		_ = binary.Write(&b.tensors, binary.LittleEndian, d)
	}
	_ = binary.Write(&b.tensors, binary.LittleEndian, typ)
	_ = binary.Write(&b.tensors, binary.LittleEndian, uint64(0))
	b.nTensor++
}

func (b *headerBuilder) bytes() []byte {
	var out bytes.Buffer
	out.WriteString("GGUF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(3))
	_ = binary.Write(&out, binary.LittleEndian, b.nTensor)
	_ = binary.Write(&out, binary.LittleEndian, b.nKV)
	out.Write(b.kvs.Bytes())
	out.Write(b.tensors.Bytes())
	return out.Bytes()
}
