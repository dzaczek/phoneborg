// Package memplan is the pure memory-sizing arithmetic shared by the node
// agent (node-agent/sizing.go, which picks a context size, slot count and KV
// cache type that fit a phone's measured budget before switching models) and
// the controller's placement planner (controller/models, which checks
// whether a model fits a node's reported budget before assigning it there).
// One shared implementation means the plan and the agent's own sizing always
// agree (ADR-011, ADR-012).
package memplan

// MinCtx is the floor context size Plan halves the context down to before it
// starts reducing slots instead.
const MinCtx = 4096

const mib int64 = 1 << 20

// SparseAllowanceFrac is the fraction of a model's sparse (row-accessed)
// tensor bytes counted toward resident memory, for the mmap pages a session
// actually touches (docs/DECISIONS.md ADR-012 addendum). Measured on a Mi 8,
// Gemma 3n E2B (2886 MiB file, 1440 MiB sparse per_layer_token_embd.weight)
// used 1774 MiB of RSS; 10% of the sparse bytes is a documented, deliberately
// conservative approximation, not a precise fit to that one measurement.
const SparseAllowanceFrac = 0.10

// ResidentWeightBytes splits fileSize into the bytes counted fully resident
// and the sparse bytes read only a few rows of per token (ADR-012 addendum).
// residentBytes is normally the catalog/DesiredRuntime's resident_bytes; 0 or
// a value above fileSize means unknown (an older controller, or a model with
// no computed metadata), so the whole file counts as resident, matching
// sizing before sparse-tensor accounting existed.
func ResidentWeightBytes(fileSize, residentBytes int64) (resident, sparse int64) {
	if residentBytes <= 0 || residentBytes > fileSize {
		return fileSize, 0
	}
	return residentBytes, fileSize - residentBytes
}

// Shape is the GGUF metadata needed to estimate KV-cache memory.
type Shape struct {
	Layers  int
	KVHeads int
	HeadDim int
}

// BytesPerElement is the KV cache element size for a cache type. q8_0 stores
// values in blocks of 32 with one f16 scale per block: (32 + 2) / 32 =
// 1.0625 bytes/element.
func BytesPerElement(kvType string) float64 {
	if kvType == "q8_0" {
		return 1.0625
	}
	return 2 // f16
}

// BytesPerToken is the combined K+V cache size for one token, across every
// layer and KV head, at the given cache type.
func BytesPerToken(shape Shape, kvType string) int64 {
	return int64(2 * float64(shape.Layers) * float64(shape.KVHeads) * float64(shape.HeadDim) * BytesPerElement(kvType))
}

// NeedBytes estimates total RAM: the resident weights (residentBytes when
// known, else the whole file, see ResidentWeightBytes) plus an allowance for
// sparse pages actually touched, the KV cache for every slot, and a fixed
// overhead for llama-server's own allocations.
func NeedBytes(fileSize, residentBytes int64, ctx, slots int, shape Shape, kvType string) int64 {
	resident, sparse := ResidentWeightBytes(fileSize, residentBytes)
	return resident + int64(float64(sparse)*SparseAllowanceFrac) + int64(slots)*int64(ctx)*BytesPerToken(shape, kvType) + 150*mib
}

// KVCandidates lists the cache types to try, in order, for a requested
// kv_type. "auto" (or anything unrecognized) tries f16 first, then q8_0.
func KVCandidates(kvType string) []string {
	switch kvType {
	case "f16", "q8_0":
		return []string{kvType}
	default:
		return []string{"f16", "q8_0"}
	}
}

// Result is the configuration Plan found: the ctx/slots/KV type that fits
// (Fits true), or, when nothing fits, the smallest configuration it tried
// and how much RAM that still needs.
type Result struct {
	Ctx   int
	Slots int
	KV    string // resolved: "f16" or "q8_0", never "auto"
	Need  int64
	Fits  bool
}

// Plan searches for a context size, slot count and KV cache type that fit
// budget bytes, trying configurations in the same order the node agent
// degrades a model switch (ADR-012): the requested ctx/slots with each KV
// candidate, then ctx halved down to MinCtx, then 1 slot. residentBytes is
// the model's resident bytes (0 = unknown, use fileSize; ADR-012 addendum).
func Plan(fileSize, residentBytes int64, shape Shape, ctx, slots int, kvType string, budget int64) Result {
	if ctx <= 0 {
		ctx = MinCtx
	}
	if slots <= 0 {
		slots = 1
	}
	kvs := KVCandidates(kvType)

	try := func(ctx, slots int) (string, int64, bool) {
		for _, kv := range kvs {
			need := NeedBytes(fileSize, residentBytes, ctx, slots, shape, kv)
			if need <= budget {
				return kv, need, true
			}
		}
		return "", 0, false
	}

	if kv, need, ok := try(ctx, slots); ok {
		return Result{Ctx: ctx, Slots: slots, KV: kv, Need: need, Fits: true}
	}
	for ctx > MinCtx {
		ctx /= 2
		if ctx < MinCtx {
			ctx = MinCtx
		}
		if kv, need, ok := try(ctx, slots); ok {
			return Result{Ctx: ctx, Slots: slots, KV: kv, Need: need, Fits: true}
		}
	}
	if slots > 1 {
		slots = 1
		if kv, need, ok := try(ctx, slots); ok {
			return Result{Ctx: ctx, Slots: slots, KV: kv, Need: need, Fits: true}
		}
	}
	return Result{Ctx: ctx, Slots: slots, KV: kvs[len(kvs)-1], Need: NeedBytes(fileSize, residentBytes, ctx, slots, shape, kvs[len(kvs)-1])}
}
