package main

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Not parallel: the soft memory limit belongs to the whole process.

func TestQueryCacheTunesTheSoftMemoryLimit(t *testing.T) {
	const unlimited = 1 << 62
	before := debug.SetMemoryLimit(unlimited)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	// Kept alive through the test, so the last collection counted it as live.
	held := make([]byte, 64<<20)
	runtime.GC()
	defer runtime.KeepAlive(held)

	tests := []struct {
		name  string
		cache *queryCache
		// The limit set lies in [min, max].
		min, max int64
	}{
		{name: "no cache", cache: nil, min: unlimited, max: unlimited},
		{name: "a cache without a limit", cache: new(queryCache), min: unlimited, max: unlimited},
		{name: "a limit above what is held", cache: &queryCache{memoryLimit: 1 << 40}, min: 1 << 40, max: 1 << 40},
		// 64 MiB are held, so the limit rises to half as much again: at least
		// 96 MiB, and far less than the gigabytes a wrong unit would give.
		{name: "a limit below what is held", cache: &queryCache{memoryLimit: 1 << 20}, min: 96 << 20, max: 1 << 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debug.SetMemoryLimit(unlimited)
			tt.cache.tuneMemoryLimit()
			if got := debug.SetMemoryLimit(-1); got < tt.min || got > tt.max {
				t.Errorf("tuneMemoryLimit() left a soft limit of %d bytes, want within [%d, %d]", got, tt.min, tt.max)
			}
		})
	}
}
