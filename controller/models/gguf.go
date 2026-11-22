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

// Tokenizer is the part of a GGUF's tokenizer that decides whether one model
// can draft tokens for another.
type Tokenizer struct {
	Model    string // tokenizer.ggml.model
	Vocab    int
	BOS, EOS int64 // -1 = unset
}

// TokenizerOf reads a GGUF file's tokenizer metadata, like ReadMeta.
func TokenizerOf(path string) (t Tokenizer, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, err = Tokenizer{}, fmt.Errorf("not a readable GGUF file: %v", r)
		}
	}()
	gf, err := gguf.ParseGGUFFile(path, gguf.SkipLargeMetadata())
	if err != nil {
		return Tokenizer{}, fmt.Errorf("not a readable GGUF file: %w", err)
	}
	gt := gf.Tokenizer()
	return Tokenizer{Model: gt.Model, Vocab: int(gt.TokensLength), BOS: gt.BOSTokenID, EOS: gt.EOSTokenID}, nil
}

// maxDraftVocabDiff is llama.cpp's SPEC_VOCAB_MAX_SIZE_DIFFERENCE.
const maxDraftVocabDiff = 128

// DraftCompatible reports why a model with tokenizer draft cannot draft for
// one with tokenizer main; llama-server's own check, so a bad pair fails
// when the pool is set rather than as a head crash.
func DraftCompatible(main, draft Tokenizer) error {
	switch {
	case main.Model != draft.Model:
		return fmt.Errorf("tokenizer %q differs from %q", draft.Model, main.Model)
	case main.BOS != draft.BOS || main.EOS != draft.EOS:
		return fmt.Errorf("BOS/EOS token ids differ (%d/%d vs %d/%d)", draft.BOS, draft.EOS, main.BOS, main.EOS)
	case main.Vocab-draft.Vocab > maxDraftVocabDiff || draft.Vocab-main.Vocab > maxDraftVocabDiff:
		return fmt.Errorf("vocabulary sizes %d and %d differ by more than %d", draft.Vocab, main.Vocab, maxDraftVocabDiff)
	}
	return nil
}
