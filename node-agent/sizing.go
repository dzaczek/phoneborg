package nodeagent

import "fmt"

const mib int64 = 1 << 20

// minCtx is the floor PlanMemory halves the context down to before it starts
// reducing slots instead (ADR-012).
const minCtx = 4096

// ModelShape is the GGUF metadata needed to estimate KV-cache memory.
type ModelShape struct {
	Layers  int
	KVHeads int
	HeadDim int
}

// SizingRequest is everything PlanMemory needs, decoupled from any I/O so it
// stays a pure, table-tested function.
type SizingRequest struct {
	FileSizeBytes int64
	Shape         ModelShape
	CtxSize       int    // requested context per slot; <=0 defaults to minCtx
	Slots         int    // requested parallel slots; <=0 defaults to 1
	KVType        string // "auto" | "f16" | "q8_0"; anything else is treated as "auto"
	BudgetBytes   int64
}

// SizingPlan is what the agent will actually run with.
type SizingPlan struct {
	CtxSize          int
	Slots            int
	KVType           string // resolved: "f16" or "q8_0", never "auto"
	RAMEstimateBytes int64
}

// MemoryBudget is how much RAM the agent can give a new model: what is free
// right now, plus the RssAnon (anonymous, non-file-backed memory: KV cache
// and compute buffers) of the llama-server that will be stopped for the
// switch, minus a fixed reserve for Android and the agent itself.
//
// currentServerRssAnonBytes must be RssAnon, not total RSS/VmRSS: llama-server
// mmaps the GGUF file, so most of its RSS is file-backed weight pages that
// the kernel already counts as reclaimable in MemAvailable. Adding full RSS
// on top would double-count those bytes (see ADR-012; measured on a Mi 8,
// MemAvailable barely moves when a model is loaded, confirming the weights
// are already "available").
func MemoryBudget(availBytes, currentServerRssAnonBytes uint64, reserveBytes int64) int64 {
	return int64(availBytes) + int64(currentServerRssAnonBytes) - reserveBytes
}

// bytesPerElement is the KV cache element size for a cache type. q8_0 stores
// values in blocks of 32 with one f16 scale per block: (32 + 2) / 32 =
// 1.0625 bytes/element.
func bytesPerElement(kvType string) float64 {
	if kvType == "q8_0" {
		return 1.0625
	}
	return 2 // f16
}

// bytesPerToken is the combined K+V cache size for one token, across every
// layer and KV head, at the given cache type.
func bytesPerToken(shape ModelShape, kvType string) int64 {
	return int64(2 * float64(shape.Layers) * float64(shape.KVHeads) * float64(shape.HeadDim) * bytesPerElement(kvType))
}

// needBytes estimates total RAM: the mmapped weights file, the KV cache for
// every slot, and a fixed overhead for llama-server's own allocations.
func needBytes(fileSize int64, ctx, slots int, shape ModelShape, kvType string) int64 {
	return fileSize + int64(slots)*int64(ctx)*bytesPerToken(shape, kvType) + 150*mib
}

// kvCandidates lists the cache types to try, in order, for a requested
// kv_type. "auto" (or anything unrecognized) tries f16 first, then q8_0.
func kvCandidates(kvType string) []string {
	switch kvType {
	case "f16", "q8_0":
		return []string{kvType}
	default:
		return []string{"f16", "q8_0"}
	}
}

// PlanMemory picks a context size, slot count and KV cache type that fit in
// BudgetBytes (ADR-012). It tries the requested ctx/slots with each candidate
// KV type, then halves ctx down to minCtx, then drops to 1 slot. If nothing
// fits, it returns an error naming the RAM the smallest configuration still
// needs and the available budget.
func PlanMemory(req SizingRequest) (SizingPlan, error) {
	ctx := req.CtxSize
	if ctx <= 0 {
		ctx = minCtx
	}
	slots := req.Slots
	if slots <= 0 {
		slots = 1
	}
	kvs := kvCandidates(req.KVType)

	try := func(ctx, slots int) (SizingPlan, bool) {
		for _, kv := range kvs {
			need := needBytes(req.FileSizeBytes, ctx, slots, req.Shape, kv)
			if need <= req.BudgetBytes {
				return SizingPlan{CtxSize: ctx, Slots: slots, KVType: kv, RAMEstimateBytes: need}, true
			}
		}
		return SizingPlan{}, false
	}

	if plan, ok := try(ctx, slots); ok {
		return plan, nil
	}
	for ctx > minCtx {
		ctx /= 2
		if ctx < minCtx {
			ctx = minCtx
		}
		if plan, ok := try(ctx, slots); ok {
			return plan, nil
		}
	}
	if slots > 1 {
		slots = 1
		if plan, ok := try(ctx, slots); ok {
			return plan, nil
		}
	}
	need := needBytes(req.FileSizeBytes, ctx, slots, req.Shape, kvs[len(kvs)-1])
	return SizingPlan{}, fmt.Errorf("model needs %d MiB, budget %d MiB", need/mib, req.BudgetBytes/mib)
}
