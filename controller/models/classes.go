// Package models keeps the controller's model catalog (GGUF files the
// controller downloads and serves to nodes), groups nodes into device
// classes by RAM, and plans which model each node should serve (ADR-011).
package models

const GiB = 1 << 30

// Class is a device class: nodes grouped by total RAM (Inventory.RAMTotalBytes).
type Class struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	MinRAMBytes uint64 `json:"min_ram_bytes"`
	MaxRAMBytes uint64 `json:"max_ram_bytes"` // exclusive; 0 = no upper bound
}

// Classes are ordered from the smallest to the largest RAM.
var Classes = []Class{
	{ID: "xs", Label: "< 3 GiB", MinRAMBytes: 0, MaxRAMBytes: 3 * GiB},
	{ID: "s", Label: "3–5 GiB", MinRAMBytes: 3 * GiB, MaxRAMBytes: 5 * GiB},
	{ID: "m", Label: "5–7 GiB", MinRAMBytes: 5 * GiB, MaxRAMBytes: 7 * GiB},
	{ID: "l", Label: "7–10 GiB", MinRAMBytes: 7 * GiB, MaxRAMBytes: 10 * GiB},
	{ID: "xl", Label: "10 GiB+", MinRAMBytes: 10 * GiB, MaxRAMBytes: 0},
}

// ClassOf returns the id of the class a node with ramTotal bytes belongs to.
func ClassOf(ramTotal uint64) string {
	for _, c := range Classes {
		if ramTotal >= c.MinRAMBytes && (c.MaxRAMBytes == 0 || ramTotal < c.MaxRAMBytes) {
			return c.ID
		}
	}
	return Classes[len(Classes)-1].ID
}

// IsClass reports whether id names a device class.
func IsClass(id string) bool {
	for _, c := range Classes {
		if c.ID == id {
			return true
		}
	}
	return false
}

// RAM heuristic (ADR-011). A model needs its weights (the GGUF file, mmapped
// by llama-server), an f16 KV cache for its context and some runtime
// overhead. Android itself and the apps it keeps alive take about
// AndroidBaselineBytes, whatever the phone's size.
const (
	DefaultCtxSize       = 16384
	RuntimeOverheadBytes = 150 << 20
	AndroidBaselineBytes = 2 * GiB
)

// KVCacheBytes is the size of an f16 K and V cache for ctx tokens.
func KVCacheBytes(layers, kvHeads, headDim, ctx int) int64 {
	return 2 * int64(layers) * int64(ctx) * int64(kvHeads) * int64(headDim) * 2
}

// EstimateRAM is the RAM a model needs with an f16 KV cache for ctx tokens.
func EstimateRAM(fileBytes int64, layers, kvHeads, headDim, ctx int) int64 {
	return fileBytes + KVCacheBytes(layers, kvHeads, headDim, ctx) + RuntimeOverheadBytes
}

// Fits reports whether a model needing est bytes fits on a node with
// ramTotal bytes once Android's baseline is taken off.
func Fits(est int64, ramTotal uint64) bool {
	return ramTotal > AndroidBaselineBytes && uint64(est) <= ramTotal-AndroidBaselineBytes
}

// FitsClasses lists the classes where a model needing est bytes fits on
// every node, i.e. on the smallest RAM of the class.
func FitsClasses(est int64) []string {
	out := []string{}
	for _, c := range Classes {
		if est > 0 && Fits(est, c.MinRAMBytes) {
			out = append(out, c.ID)
		}
	}
	return out
}
