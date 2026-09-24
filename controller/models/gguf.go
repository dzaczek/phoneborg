package models

import (
	"fmt"
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
	return m, nil
}
