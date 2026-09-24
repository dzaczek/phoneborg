package nodeagent

import (
	"fmt"

	"github.com/dzaczek/phoneborg/internal/memplan"
)

const mib int64 = 1 << 20

// ModelShape is the GGUF metadata needed to estimate KV-cache memory.
type ModelShape = memplan.Shape

// SizingRequest is everything PlanMemory needs, decoupled from any I/O so it
// stays a pure, table-tested function.
type SizingRequest struct {
	FileSizeBytes int64
	Shape         ModelShape
	CtxSize       int    // requested context per slot; <=0 defaults to memplan.MinCtx
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

// PlanMemory picks a context size, slot count and KV cache type that fit in
// BudgetBytes (ADR-012). It tries the requested ctx/slots with each candidate
// KV type, then halves ctx down to memplan.MinCtx, then drops to 1 slot (see
// memplan.Plan, shared with the controller's placement fit check). If
// nothing fits, it returns an error naming the RAM the smallest configuration
// still needs and the available budget.
func PlanMemory(req SizingRequest) (SizingPlan, error) {
	r := memplan.Plan(req.FileSizeBytes, req.Shape, req.CtxSize, req.Slots, req.KVType, req.BudgetBytes)
	if !r.Fits {
		return SizingPlan{}, fmt.Errorf("model needs %d MiB, budget %d MiB", r.Need/mib, req.BudgetBytes/mib)
	}
	return SizingPlan{CtxSize: r.Ctx, Slots: r.Slots, KVType: r.KV, RAMEstimateBytes: r.Need}, nil
}
