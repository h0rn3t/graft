package main

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/h0rn3t/Graft/internal/graph"
)

// A workspace query loads all of its repositories at once: every load answers
// with that directory's graph and index, and a directory the cache holds comes
// back as the same value, whoever asked first.

func TestQueryCacheLoadsDirectoriesAtOnce(t *testing.T) {
	root := t.TempDir()
	const directories = 12
	dirs := make([]string, directories)
	for i := range dirs {
		dirs[i] = filepath.Join(root, "repo"+strconv.Itoa(i), "graft")
		writeQueryCacheFixture(t, dirs[i], i+1) // i+1 nodes tell the graphs apart
	}

	var cache queryCache
	type loaded struct {
		wiring *graph.GraphV1
		index  *graph.AskIndex
		err    error
	}
	const callers = 4
	results := make([][]loaded, callers)
	var wg sync.WaitGroup
	for caller := range callers {
		results[caller] = make([]loaded, directories)
		wg.Go(func() {
			for i, dir := range dirs {
				wiring, index, err := cache.load(dir)
				results[caller][i] = loaded{wiring: wiring, index: index, err: err}
			}
		})
	}
	wg.Wait()

	for i, dir := range dirs {
		first := results[0][i]
		if first.err != nil || first.index == nil || len(first.wiring.Nodes) != i+1 {
			t.Errorf("cache.load(%q) = (%d nodes, index %v, %v), want %d nodes and an index", dir, len(first.wiring.Nodes), first.index != nil, first.err, i+1)
			continue
		}
		for caller := 1; caller < callers; caller++ {
			if got := results[caller][i]; got.wiring != first.wiring || got.index != first.index {
				t.Errorf("cache.load(%q) by caller %d = (%p, %p), want the cached (%p, %p)", dir, caller, got.wiring, got.index, first.wiring, first.index)
			}
		}
	}
}
