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
		name        string
		fileSizeMiB int64
		shape       Shape
		ctx, slots  int
		kv          string
		budgetMiB   int64
		want        Result
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan(tc.fileSizeMiB*mibTest, tc.shape, tc.ctx, tc.slots, tc.kv, tc.budgetMiB*mibTest)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
