package models

import "testing"

func TestBandwidth(t *testing.T) {
	for _, tc := range []struct {
		tokPerSec float64
		fileBytes int64
		want      float64
	}{
		{0, 1_000_000_000, 0},          // no measurement
		{-1, 1_000_000_000, 0},         // impossible
		{10, 0, 0},                     // no file size
		{10, -1, 0},                    // impossible
		{10, 1_000_000_000, 10},        // 10 tok/s * 1 GB = 10 GB/s
		{6.8, 1065 << 20, 7.593787392}, // Mi 8, Qwen2.5-1.5B (docs/DECISIONS.md ADR-015)
	} {
		if got := Bandwidth(tc.tokPerSec, tc.fileBytes); !closeEnough(got, tc.want) {
			t.Errorf("Bandwidth(%g, %d) = %g, want %g", tc.tokPerSec, tc.fileBytes, got, tc.want)
		}
	}
}

func TestPredictedTPS(t *testing.T) {
	for _, tc := range []struct {
		gbps      float64
		sizeBytes int64
		want      float64
	}{
		{0, 1_000_000_000, 0},  // node not measured yet
		{-1, 1_000_000_000, 0}, // impossible
		{7.2, 0, 0},            // no model size
		{7.2, 1_000_000_000, 7.2},
		{7.2, 2_400_000_000, 3}, // a 4B-class model on a Mi-8-speed node (ADR-015)
	} {
		if got := PredictedTPS(tc.gbps, tc.sizeBytes); !closeEnough(got, tc.want) {
			t.Errorf("PredictedTPS(%g, %d) = %g, want %g", tc.gbps, tc.sizeBytes, got, tc.want)
		}
	}
}

func TestPerfTierOf(t *testing.T) {
	for _, tc := range []struct {
		gbps float64
		want string
	}{
		{0, UnknownPerfTier},
		{-1, UnknownPerfTier},
		{0.1, "t1"},
		{3.999, "t1"},
		{4, "t2"},
		{6.9, "t2"},
		{9.999, "t2"},
		{10, "t3"},
		{24.999, "t3"},
		{25, "t4"},
		{56, "t4"}, // emulated phone on an M2 host (ADR-015)
	} {
		if got := PerfTierOf(tc.gbps); got != tc.want {
			t.Errorf("PerfTierOf(%g) = %s, want %s", tc.gbps, got, tc.want)
		}
	}
}

func TestIsPerfTier(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{{"t1", true}, {"t4", true}, {"t5", false}, {"", false}, {UnknownPerfTier, false}} {
		if got := IsPerfTier(tc.id); got != tc.want {
			t.Errorf("IsPerfTier(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func closeEnough(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
