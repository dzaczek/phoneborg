package nodeagent

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// BenchKind marks this as a placeholder score. It is only comparable with other
// synthetic-go-v0 results; llama-bench tokens/s replaces it once the native
// runtime lands.
const BenchKind = "synthetic-go-v0"

// RunBenchmark measures scalar FP32 throughput on all cores and single-thread
// memory copy bandwidth. bufBytes bounds the memory test so it is safe on
// low-RAM phones.
func RunBenchmark(d time.Duration, cores int, bufBytes int) proto.Benchmark {
	start := time.Now()
	return proto.Benchmark{
		Kind:             BenchKind,
		CPUGFLOPS:        cpuGFLOPS(d, cores),
		MemBandwidthGBps: memGBps(d, bufBytes),
		DurationMs:       time.Since(start).Milliseconds(),
	}
}

// sink absorbs the loop's result so the compiler can't prove it's dead and
// eliminate the FP work cpuGFLOPS is trying to measure.
//
//lint:ignore U1000 written-only by design, see comment above
var sink float32

func cpuGFLOPS(d time.Duration, cores int) float64 {
	if cores < 1 {
		cores = runtime.NumCPU()
	}
	var flops atomic.Uint64
	var wg sync.WaitGroup
	deadline := time.Now().Add(d)
	for i := 0; i < cores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 8 independent multiply-add chains keep the FPU pipelines busy.
			a := [8]float32{1, 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7}
			const m, c = float32(0.999999), float32(0.000001)
			var n uint64
			for time.Now().Before(deadline) {
				for k := 0; k < 4096; k++ {
					a[0] = a[0]*m + c
					a[1] = a[1]*m + c
					a[2] = a[2]*m + c
					a[3] = a[3]*m + c
					a[4] = a[4]*m + c
					a[5] = a[5]*m + c
					a[6] = a[6]*m + c
					a[7] = a[7]*m + c
				}
				n += 4096 * 8 * 2
			}
			flops.Add(n)
			sink += a[0] + a[7]
		}()
	}
	wg.Wait()
	return float64(flops.Load()) / d.Seconds() / 1e9
}

func memGBps(d time.Duration, bufBytes int) float64 {
	src := make([]byte, bufBytes/2)
	dst := make([]byte, bufBytes/2)
	for i := range src {
		src[i] = byte(i)
	}
	var moved uint64
	start := time.Now()
	for time.Since(start) < d {
		copy(dst, src)
		moved += uint64(len(src)) * 2 // read + write
	}
	return float64(moved) / time.Since(start).Seconds() / 1e9
}
