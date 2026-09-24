package models

import (
	"fmt"
	"slices"
	"strings"

	gguf "github.com/gpustack/gguf-parser-go"
)

// Meta is what the catalog keeps from a GGUF file's metadata.
type Meta struct {
	Arch     string
	Params   string // e.g. "1.5B"
	Quant    string // e.g. "Q4_K_M"
	CtxTrain int
	Layers   int
	KVHeads  int
	HeadDim  int
	License  string
	Name     string // general.name
	// SparseBytes is the total size of tensors llama.cpp accesses sparsely
	// (a few rows per token through mmap, not the whole tensor), see
	// isSparseTensor. 0 when the model has none (most models).
	SparseBytes int64
}

// sparseTensorExtra names tensors (exact match) that are read sparsely, in
// addition to the per_layer_* rule in isSparseTensor. Extend this table as
// more architectures are measured; keep it short and documented.
var sparseTensorExtra = []string{}

// isSparseTensor reports whether llama.cpp reads tensor name only a few rows
// of per token (an embedding-lookup table, mmap'd and indexed by token id),
// rather than the whole tensor, so its bytes should not count as resident
// memory (docs/DECISIONS.md ADR-012 addendum).
//
// Gemma 3n's per-layer embedding table (per_layer_token_embd.weight) is the
// measured case: llama.cpp looks up one row per layer per token and never
// reads the rest, so its 1440 MiB of a 2886 MiB file stays on cold mmap
// pages while the file's on-disk size would count it as resident. Plain
// token_embd.weight is deliberately never matched, even by the extra table:
// when a model ties its input and output embeddings (no separate
// output.weight), the output layer reads token_embd.weight in full on every
// forward pass, so treating it as sparse would undercount resident memory.
// Keeping the rule to per_layer_* only avoids having to detect that case.
func isSparseTensor(name string) bool {
	if strings.HasPrefix(name, "per_layer_") && strings.HasSuffix(name, "token_embd.weight") {
		return true
	}
	return slices.Contains(sparseTensorExtra, name)
}

// sparseBytes sums the on-disk bytes of every tensor in tensors that
// isSparseTensor matches.
func sparseBytes(tensors gguf.GGUFTensorInfos) int64 {
	var sum uint64
	for _, ti := range tensors {
		if isSparseTensor(ti.Name) {
			sum += ti.Bytes()
		}
	}
	return int64(sum)
}

// ReadMeta reads a GGUF file's header with gguf-parser-go. It reads the
// metadata and tensor index only, not the weights. The file comes from
// the network, so a parser panic on a malformed file becomes an error.
func ReadMeta(path string) (m Meta, err error) {
	defer func() {
		if r := recover(); r != nil {
			m, err = Meta{}, fmt.Errorf("not a readable GGUF file: %v", r)
		}
	}()
	gf, err := gguf.ParseGGUFFile(path, gguf.SkipLargeMetadata())
	if err != nil {
		return Meta{}, fmt.Errorf("not a readable GGUF file: %w", err)
	}
	md, a := gf.Metadata(), gf.Architecture()
	m = Meta{
		Arch:     md.Architecture,
		License:  md.License,
		Name:     md.Name,
		CtxTrain: int(a.MaximumContextLength),
		Layers:   int(a.BlockCount),
		KVHeads:  int(a.AttentionHeadCountKV),
		HeadDim:  int(a.AttentionKeyLength),
	}
	if m.KVHeads == 0 {
		m.KVHeads = int(a.AttentionHeadCount) // no grouped-query attention
	}
	if m.HeadDim == 0 && a.AttentionHeadCount > 0 {
		m.HeadDim = int(a.EmbeddingLength / a.AttentionHeadCount)
	}
	if md.FileTypeDescriptor != "Unknown" {
		m.Quant = md.FileTypeDescriptor
	}
	if kv, ok := gf.Header.MetadataKV.Get("general.size_label"); ok && kv.ValueType == gguf.GGUFMetadataValueTypeString {
		m.Params = kv.ValueString()
	} else if md.Parameters > 0 {
		m.Params = strings.ReplaceAll(md.Parameters.String(), " ", "")
	}
	m.SparseBytes = sparseBytes(gf.TensorInfos)
	return m, nil
}
