package nodeagent

import (
	"strings"
	"testing"
)

func TestMemoryBudget(t *testing.T) {
	// Xiaomi Mi 8: ~2.8 GiB MemAvailable while a 720 MiB llama-server runs,
	// default 600 MiB reserve.
	got := MemoryBudget(2800*uint64(mib), 720*uint64(mib), 600*mib)
	want := 2920 * mib
	if got != want {
		t.Fatalf("MemoryBudget = %d, want %d", got, want)
	}
}

func TestPlanMemory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     SizingRequest
		want    SizingPlan
		wantErr string
	}{
		{
			// Qwen2.5-0.5B: 469 MiB file, 24 layers, 2 KV heads, head_dim 64.
			// Fits comfortably at the requested ctx/slots with f16.
			name: "0.5B fits with f16 at requested ctx",
			req: SizingRequest{
				FileSizeBytes: 469 * mib,
				Shape:         ModelShape{Layers: 24, KVHeads: 2, HeadDim: 64},
				CtxSize:       4096,
				Slots:         1,
				KVType:        "auto",
				BudgetBytes:   2920 * mib, // Mi 8 budget from TestMemoryBudget
			},
			want: SizingPlan{CtxSize: 4096, Slots: 1, KVType: "f16", RAMEstimateBytes: 699400192},
		},
		{
			// Qwen2.5-1.5B: ~1.1 GiB file, 28 layers, 2 KV heads, head_dim 128.
			// f16 needs 2173 MiB (over budget); q8_0 needs 1753 MiB (fits) at
			// the same ctx/slots, so only the KV type changes.
			name: "1.5B needs q8_0 at requested ctx",
			req: SizingRequest{
				FileSizeBytes: 1127 * mib,
				Shape:         ModelShape{Layers: 28, KVHeads: 2, HeadDim: 128},
				CtxSize:       16384,
				Slots:         2,
				KVType:        "auto",
				BudgetBytes:   2000 * mib,
			},
			want: SizingPlan{CtxSize: 16384, Slots: 2, KVType: "q8_0", RAMEstimateBytes: 1838153728},
		},
		{
			// A 3B model (36 layers, 2 KV heads, head_dim 128, ~1.9 GiB file)
			// on a tighter budget: neither KV type fits at the requested
			// 16384 ctx / 2 slots, nor after halving ctx to the 4096 floor at
			// 2 slots. Only q8_0 at 1 slot fits.
			name: "3B needs q8_0, floor ctx and 1 slot",
			req: SizingRequest{
				FileSizeBytes: 1946 * mib,
				Shape:         ModelShape{Layers: 36, KVHeads: 2, HeadDim: 128},
				CtxSize:       16384,
				Slots:         2,
				KVType:        "auto",
				BudgetBytes:   2200 * mib,
			},
			want: SizingPlan{CtxSize: 4096, Slots: 1, KVType: "q8_0", RAMEstimateBytes: 2278031360},
		},
		{
			// Same 3B model, budget too small even for the most degraded
			// configuration: State must go to "error" naming the shortfall.
			name: "3B does not fit at any size",
			req: SizingRequest{
				FileSizeBytes: 1946 * mib,
				Shape:         ModelShape{Layers: 36, KVHeads: 2, HeadDim: 128},
				CtxSize:       16384,
				Slots:         2,
				KVType:        "auto",
				BudgetBytes:   2000 * mib,
			},
			wantErr: "model needs 2172 MiB, budget 2000 MiB",
		},
		{
			// Same model and budget as "1.5B needs q8_0 at requested ctx",
			// but KVType is pinned to f16: PlanMemory must not fall back to
			// q8_0, so it halves ctx to 8192 (fits at f16) instead.
			name: "fixed f16 degrades ctx instead of falling back to q8_0",
			req: SizingRequest{
				FileSizeBytes: 1127 * mib,
				Shape:         ModelShape{Layers: 28, KVHeads: 2, HeadDim: 128},
				CtxSize:       16384,
				Slots:         2,
				KVType:        "f16",
				BudgetBytes:   2000 * mib,
			},
			want: SizingPlan{CtxSize: 8192, Slots: 2, KVType: "f16", RAMEstimateBytes: 1808793600},
		},
		{
			name: "zero ctx and slots default to minCtx and 1",
			req: SizingRequest{
				FileSizeBytes: 469 * mib,
				Shape:         ModelShape{Layers: 24, KVHeads: 2, HeadDim: 64},
				KVType:        "auto",
				BudgetBytes:   2920 * mib,
			},
			want: SizingPlan{CtxSize: 4096, Slots: 1, KVType: "f16", RAMEstimateBytes: 699400192},
		},
		{
			// Gemma 3n E2B it (docs/REAL_PHONES.md, ADR-012 addendum): 2886
			// MiB file, but only 1446 MiB resident (1440 MiB is a
			// per_layer_token_embd.weight table read sparsely through mmap,
			// measured RSS on a Mi 8 was 1774 MiB). Without ResidentBytes the
			// agent refused this model ("model needs 3164 MiB, budget 2853
			// MiB"); with it, it fits at the requested 16k context.
			name: "Gemma 3n E2B fits once resident bytes exclude the sparse table",
			req: SizingRequest{
				FileSizeBytes: 2886 * mib,
				ResidentBytes: 1446 * mib,
				Shape:         ModelShape{Layers: 35, KVHeads: 2, HeadDim: 256},
				CtxSize:       16384,
				Slots:         1,
				KVType:        "auto",
				BudgetBytes:   2853 * mib,
			},
			want: SizingPlan{CtxSize: 16384, Slots: 1, KVType: "q8_0", RAMEstimateBytes: 2448424960},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanMemory(tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
