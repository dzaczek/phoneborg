package models

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/controller/models/modeltest"
)

func TestReadMeta(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.gguf")
	os.WriteFile(p, modeltest.GGUF(modeltest.LlamaLike("qwen2", 28, 12, 2, 1536, 32768), 0), 0o600)
	m, err := ReadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	want := Meta{Arch: "qwen2", Params: "1.5B", Quant: "Q4_K_M", CtxTrain: 32768, Layers: 28, KVHeads: 2, HeadDim: 128,
		License: "apache-2.0", Name: "Test Model"}
	if m != want {
		t.Fatalf("meta = %+v\nwant   %+v", m, want)
	}
}

// TestReadMetaComputesSparseBytes covers ADR-012's addendum: a
// per_layer_token_embd.weight tensor (Gemma 3n's per-layer embedding table)
// counts toward SparseBytes, but the plain token_embd.weight next to it does
// not.
func TestReadMetaComputesSparseBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.gguf")
	data := modeltest.GGUFTensors(modeltest.LlamaLike("gemma3n", 4, 8, 2, 256, 32768), []modeltest.Tensor{
		{Name: "token_embd.weight", Elements: 8},
		{Name: "per_layer_token_embd.weight", Elements: 1024},
	}, 0)
	os.WriteFile(p, data, 0o600)
	m, err := ReadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1024 * 4); m.SparseBytes != want {
		t.Fatalf("SparseBytes = %d, want %d (token_embd.weight must not count)", m.SparseBytes, want)
	}
}

func TestIsSparseTensor(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"per_layer_token_embd.weight", true},
		{"token_embd.weight", false}, // tied embeddings: read fully by the output layer
		{"output.weight", false},
		{"blk.0.attn_q.weight", false},
		{"model.per_layer_token_embd.weight", false}, // must start with per_layer_, not just contain it
	} {
		if got := isSparseTensor(tc.name); got != tc.want {
			t.Errorf("isSparseTensor(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReadMetaRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"html":      []byte("<html>not found</html>"),
		"truncated": modeltest.GGUF(modeltest.LlamaLike("llama", 16, 32, 8, 2048, 131072), 0)[:40],
		"empty":     nil,
	} {
		p := filepath.Join(dir, name+".gguf")
		os.WriteFile(p, data, 0o600)
		if _, err := ReadMeta(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClassesAndFit(t *testing.T) {
	for _, tc := range []struct {
		ram  uint64
		want string
	}{{0, "xs"}, {2 * GiB, "xs"}, {3*GiB - 1, "xs"}, {3 * GiB, "s"}, {5*GiB + 1, "m"}, {7 * GiB, "l"}, {10*GiB - 1, "l"}, {10 * GiB, "xl"}, {64 * GiB, "xl"}} {
		if got := ClassOf(tc.ram); got != tc.want {
			t.Errorf("ClassOf(%d) = %s, want %s", tc.ram, got, tc.want)
		}
	}
	// Qwen2.5-0.5B: 24 layers, 2 KV heads of 64: 192 MiB of f16 KV at 16k.
	if kv := KVCacheBytes(24, 2, 64, 16384); kv != 192<<20 {
		t.Fatalf("kv = %d", kv)
	}
	est := EstimateRAM(400<<20, 24, 2, 64, 16384) // 742 MiB
	if !Fits(est, 3*GiB) || Fits(est, 2*GiB) || Fits(est, GiB) {
		t.Fatal("fit")
	}
	if got := FitsClasses(est); len(got) != 4 || got[0] != "s" {
		t.Fatalf("fits = %v", got)
	}
	if got := FitsClasses(5 * GiB); len(got) != 2 || got[0] != "l" {
		t.Fatalf("fits 5 GiB = %v", got)
	}
}

func tok(model string, vocab int, bos, eos int64) Tokenizer {
	return Tokenizer{Model: model, Vocab: vocab, BOS: bos, EOS: eos}
}

func TestDraftCompatible(t *testing.T) {
	main := tok("gpt2", 151936, 11, 12)
	for _, tc := range []struct {
		name  string
		draft Tokenizer
		err   string
	}{
		{"same", tok("gpt2", 151936, 11, 12), ""},
		{"vocab 64 apart", tok("gpt2", 151936-64, 11, 12), ""},
		{"vocab 128 apart", tok("gpt2", 151936+128, 11, 12), ""},
		{"vocab 129 smaller", tok("gpt2", 151936-129, 11, 12), "differ by more than 128"},
		{"vocab 129 larger", tok("gpt2", 151936+129, 11, 12), "differ by more than 128"},
		{"other tokenizer", tok("llama", 151936, 11, 12), "tokenizer"},
		{"BOS differs", tok("gpt2", 151936, 1, 12), "BOS/EOS"},
		{"EOS differs", tok("gpt2", 151936, 11, 2), "BOS/EOS"},
	} {
		err := DraftCompatible(main, tc.draft)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.err)
		}
	}
}

// TokenizerOf reads the vocabulary size from a header parsed with
// SkipLargeMetadata, so it needs no second pass over the file.
func TestTokenizerOf(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.gguf")
	tokens := make([]string, 300)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("t%d", i)
	}
	kvs := append(modeltest.LlamaLike("qwen2", 28, 12, 2, 1536, 32768),
		modeltest.KV{Key: "tokenizer.ggml.model", Value: "gpt2"}, modeltest.KV{Key: "tokenizer.ggml.tokens", Value: tokens},
		modeltest.KV{Key: "tokenizer.ggml.bos_token_id", Value: uint32(5)}, modeltest.KV{Key: "tokenizer.ggml.eos_token_id", Value: uint32(6)})
	os.WriteFile(p, modeltest.GGUF(kvs, 0), 0o600)
	got, err := TokenizerOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := tok("gpt2", 300, 5, 6); got != want {
		t.Fatalf("tokenizer = %+v, want %+v", got, want)
	}
	os.WriteFile(p, []byte("garbage"), 0o600)
	if _, err := TokenizerOf(p); err == nil {
		t.Error("garbage accepted")
	}
}
