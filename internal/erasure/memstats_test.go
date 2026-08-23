package erasure

import "runtime"

type testingMemStats = runtime.MemStats

func readMem(m *runtime.MemStats) {
	runtime.GC()
	runtime.ReadMemStats(m)
}
