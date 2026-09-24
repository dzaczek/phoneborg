// Package modeltest writes tiny GGUF files for tests: a header with the
// given metadata and one small F32 tensor, so the parser sees a valid file.
package modeltest

import (
	"bytes"
	"encoding/binary"
)

// KV is one metadata entry. Value may be string, uint32 or uint64.
type KV struct {
	Key   string
	Value any
}

// LlamaLike returns metadata for a llama-style model.
func LlamaLike(arch string, layers, heads, kvHeads, embd, ctxTrain uint32) []KV {
	return []KV{
		{"general.architecture", arch},
		{"general.name", "Test Model"},
		{"general.size_label", "1.5B"},
		{"general.license", "apache-2.0"},
		{"general.file_type", uint32(15)}, // MOSTLY_Q4_K_M
		{arch + ".context_length", ctxTrain},
		{arch + ".block_count", layers},
		{arch + ".embedding_length", embd},
		{arch + ".attention.head_count", heads},
		{arch + ".attention.head_count_kv", kvHeads},
	}
}

// Tensor is one 1-D F32 tensor written by GGUFTensors.
type Tensor struct {
	Name     string
	Elements uint64
}

// GGUF encodes a GGUF v3 file with kvs and an F32 tensor of 8 elements named
// "token_embd.weight", followed by pad zero bytes (to make files of a chosen
// size).
func GGUF(kvs []KV, pad int) []byte {
	return GGUFTensors(kvs, []Tensor{{Name: "token_embd.weight", Elements: 8}}, pad)
}

// GGUFTensors is like GGUF but writes every tensor in tensors instead of the
// single default one, for tests that need more than one tensor (e.g.
// sparse-tensor accounting, which needs a "per_layer_token_embd.weight"
// tensor alongside the usual "token_embd.weight").
func GGUFTensors(kvs []KV, tensors []Tensor, pad int) []byte {
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { le(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	le(uint32(3))            // version
	le(uint64(len(tensors))) // tensors
	le(uint64(len(kvs)))     // metadata entries
	for _, kv := range kvs {
		str(kv.Key)
		switch v := kv.Value.(type) {
		case string:
			le(uint32(8))
			str(v)
		case uint32:
			le(uint32(4))
			le(v)
		case uint64:
			le(uint32(10))
			le(v)
		default:
			panic("modeltest: unsupported value type")
		}
	}
	var offset uint64
	for _, t := range tensors {
		str(t.Name)
		le(uint32(1))  // dims
		le(t.Elements) // elements
		le(uint32(0))  // F32
		le(offset)     // offset in the data section
		offset += t.Elements * 4
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, offset+uint64(pad)))
	return b.Bytes()
}
