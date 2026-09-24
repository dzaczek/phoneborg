package memplan

import "testing"

const mibTest int64 = 1 << 20

// Test shapes mirror node-agent/sizing_test.go's ADR-012 measurements, so
// both packages are checked against the same numbers.
var (
	shapeA = Shape{Layers: 24, KVHeads: 2, HeadDim: 64}  // Qwen2.5-0.5B, 469 MiB file
	shapeB = Shape{Layers: 28, KVHeads: 2, HeadDim: 128} // Qwen2.5-1.5B, 1127 MiB file
)

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name             string
		fileSizeMiB      int64
		residentBytesMiB int64
		shape            Shape
		ctx, slots       int
		kv               string
		budgetMiB        int64
		want             Result
	}{
		{
			name: "fits with f16 at requested ctx", fileSizeMiB: 469, shape: shapeA,
			ctx: 4096, slots: 1, kv: "auto", budgetMiB: 2920,
			want: Result{Ctx: 4096, Slots: 1, KV: "f16", Need: 699400192, Fits: true},
		},
		{
			// f16 needs 2173 MiB (over budget); q8_0 needs 1753 MiB (fits) at
			// the same ctx/slots, so only the KV type changes.
			name: "falls back to q8_0 at requested ctx", fileSizeMiB: 1127, shape: shapeB,
			ctx: 16384, slots: 2, kv: "auto", budgetMiB: 2000,
			want: Result{Ctx: 16384, Slots: 2, KV: "q8_0", Need: 1838153728, Fits: true},
		},
		{
			// Even q8_0 does not fit at the requested ctx (1753 MiB > 1700
			// MiB budget); halving ctx to 8192 makes q8_0 fit (1515 MiB).
			name: "halves ctx when q8_0 alone is not enough", fileSizeMiB: 1127, shape: shapeB,
			ctx: 16384, slots: 2, kv: "auto", budgetMiB: 1700,
			want: Result{Ctx: 8192, Slots: 2, KV: "q8_0", Need: 1588592640, Fits: true},
		},
		{
			// Nothing fits, even at the MinCtx floor: Fits is false and Need
			// names the smallest (q8_0, the last KV candidate) configuration.
			name: "nothing fits reports the smallest configuration tried", fileSizeMiB: 469, shape: shapeA,
			ctx: 4096, slots: 1, kv: "auto", budgetMiB: 500,
			want: Result{Ctx: 4096, Slots: 1, KV: "q8_0", Need: 675807232, Fits: false},
		},
		{
			// Gemma 3n E2B it (docs/BENCHMARKS.md, ADR-012 addendum): 2886
			// MiB file, 1440 MiB of it a per_layer_token_embd.weight table
			// read sparsely, so resident bytes are 1446 MiB. Before resident-
			// bytes accounting this needed the whole 2886 MiB file plus KV
			// plus overhead and did not fit a 2853 MiB Mi 8 budget at any
			// degradation step, even though it measured at only 1774 MiB RSS.
			// The architecture shape below is an approximation (block_count
			// 35, kv_heads 2, key_length 256) since the real GGUF header was
			// not captured in this repo; what matters is that resident-bytes
			// accounting makes it fit at the requested 16k context.
			name:        "Gemma 3n E2B fits once sparse per-layer embeddings are excluded",
			fileSizeMiB: 2886, residentBytesMiB: 1446, shape: Shape{Layers: 35, KVHeads: 2, HeadDim: 256},
			ctx: 16384, slots: 1, kv: "auto", budgetMiB: 2853,
			want: Result{Ctx: 16384, Slots: 1, KV: "q8_0", Need: 2448424960, Fits: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan(tc.fileSizeMiB*mibTest, tc.residentBytesMiB*mibTest, tc.shape, tc.ctx, tc.slots, tc.kv, tc.budgetMiB*mibTest)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got.Fits && got.Need > tc.budgetMiB*mibTest {
				t.Fatalf("Need %d exceeds budget %d despite Fits=true", got.Need, tc.budgetMiB*mibTest)
			}
		})
	}
}

func TestResidentWeightBytes(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		fileSize, residentBytes  int64
		wantResident, wantSparse int64
	}{
		{"unknown (0) counts the whole file as resident", 2886 * mibTest, 0, 2886 * mibTest, 0},
		{"no sparse tensors: resident equals the file", 1127 * mibTest, 1127 * mibTest, 1127 * mibTest, 0},
		{"Gemma 3n E2B: 1440 MiB sparse", 2886 * mibTest, 1446 * mibTest, 1446 * mibTest, 1440 * mibTest},
		{"a bogus resident above the file size is treated as unknown", 100 * mibTest, 200 * mibTest, 100 * mibTest, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resident, sparse := ResidentWeightBytes(tc.fileSize, tc.residentBytes)
			if resident != tc.wantResident || sparse != tc.wantSparse {
				t.Fatalf("ResidentWeightBytes(%d, %d) = (%d, %d), want (%d, %d)",
					tc.fileSize, tc.residentBytes, resident, sparse, tc.wantResident, tc.wantSparse)
			}
		})
	}
}
