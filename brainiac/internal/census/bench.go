package census

import (
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// memBench copies buffers totalling about totalBytes (split across threads,
// far larger than the CPU caches together) for about dur, and reports bytes
// read plus written per second. The memory is handed back to the OS after.
func memBench(totalBytes, threads int, dur time.Duration) Bench {
	if threads < 1 {
		threads = 1
	}
	if threads > 32 {
		threads = 32
	}
	per := totalBytes / (2 * threads)
	if per < 1<<20 {
		per = 1 << 20
	}

	type pair struct{ src, dst []byte }
	bufs := make([]pair, threads)
	for i := range bufs {
		bufs[i] = pair{make([]byte, per), make([]byte, per)}
		for j := 0; j < per; j += 4096 {
			bufs[i].src[j] = byte(j) // touch every page so the copy measures memory, not page faults
		}
		copy(bufs[i].dst, bufs[i].src)
	}

	var moved atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(dur)
	for i := range bufs {
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				copy(p.dst, p.src)
				moved.Add(int64(2 * len(p.src)))
			}
		}(bufs[i])
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	bufs = nil
	runtime.GC()
	debug.FreeOSMemory()

	gbps := 0.0
	if elapsed > 0 {
		gbps = round2(float64(moved.Load()) / elapsed / 1e9)
	}
	return Bench{MemCopyGBps: gbps, Threads: threads, Seconds: round2(elapsed)}
}
