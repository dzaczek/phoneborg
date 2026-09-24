package models

// Performance tiers from measured memory bandwidth (ADR-015): on phones,
// token generation is memory-bandwidth bound, so gen_tok_s x
// model_file_bytes is roughly constant per phone ("effective bandwidth").
// This lets the controller classify a phone's *speed*, not just its RAM, and
// keeps the planner from placing a model a phone can run but only uselessly
// slowly.

// UnknownPerfTier is returned when a node has not measured its bandwidth yet.
const UnknownPerfTier = "?"

// PerfTier is a node performance class by measured generation bandwidth.
type PerfTier struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	MinGBps float64 `json:"min_gbps"`
	MaxGBps float64 `json:"max_gbps"` // exclusive; 0 = no upper bound
}

// PerfTiers are ordered from the slowest to the fastest.
var PerfTiers = []PerfTier{
	{ID: "t1", Label: "< 4 GB/s", MinGBps: 0, MaxGBps: 4},
	{ID: "t2", Label: "4–10 GB/s", MinGBps: 4, MaxGBps: 10},
	{ID: "t3", Label: "10–25 GB/s", MinGBps: 10, MaxGBps: 25},
	{ID: "t4", Label: "25 GB/s+", MinGBps: 25, MaxGBps: 0},
}

// IsPerfTier reports whether id names a performance tier.
func IsPerfTier(id string) bool {
	for _, t := range PerfTiers {
		if t.ID == id {
			return true
		}
	}
	return false
}

// PerfTierOf returns the tier id for a node measuring genGBps of generation
// bandwidth, or UnknownPerfTier when it has not been measured yet (<= 0).
func PerfTierOf(genGBps float64) string {
	if genGBps <= 0 {
		return UnknownPerfTier
	}
	for _, t := range PerfTiers {
		if genGBps >= t.MinGBps && (t.MaxGBps == 0 || genGBps < t.MaxGBps) {
			return t.ID
		}
	}
	return PerfTiers[len(PerfTiers)-1].ID
}

// Perf is a node's last known-good measured memory bandwidth (ADR-015). Zero
// fields mean "not measured yet".
type Perf struct {
	GenGBps    float64 `json:"gen_gbps,omitempty"`
	PromptGBps float64 `json:"prompt_gbps,omitempty"`
}

// Bandwidth computes GB/s from a measured tokens/s and the served model's
// resident bytes (ADR-015; ADR-012 addendum: callers pass resident bytes
// when known, else the file size). 0 when either input is unknown.
func Bandwidth(tokPerSec float64, residentBytes int64) float64 {
	if tokPerSec <= 0 || residentBytes <= 0 {
		return 0
	}
	return tokPerSec * float64(residentBytes) / 1e9
}

// PredictedTPS predicts a model's tokens/s from a node's measured bandwidth
// (gbps) and the target model's resident bytes (falling back to the file
// size when unknown, ADR-012 addendum). 0 when either input is unknown.
func PredictedTPS(gbps float64, residentBytes int64) float64 {
	if gbps <= 0 || residentBytes <= 0 {
		return 0
	}
	return gbps * 1e9 / float64(residentBytes)
}
