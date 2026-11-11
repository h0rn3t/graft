package main

import (
	"cmp"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"

	"github.com/h0rn3t/Graft/internal/graph"
)

// queryCacheBudget bounds the decoded graphs and ask indexes one server keeps,
// as estimated by queryCacheEntry.size.
const queryCacheBudget = 256 << 20

// mcpMemoryLimit is the soft memory limit an MCP server runs under: what
// queryCacheBudget keeps and what a workspace query decodes at once, with room
// to spare. Without a limit the collector lets the heap double before it runs,
// and the graphs a workspace query decodes and drops made a server of 40
// repositories peak at 1 GiB while it kept under 400 MiB.
const mcpMemoryLimit = 640 << 20

// tuneMemoryLimit sets the process's soft memory limit to cache.memoryLimit,
// or to half as much again as the heap in use when that is more: a limit under
// what the server has to keep would only make the collector run without pause.
// A cache with no memoryLimit leaves the runtime alone.
func (cache *queryCache) tuneMemoryLimit() {
	if cache == nil || cache.memoryLimit == 0 {
		return
	}
	live := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(live)
	if live[0].Value.Kind() != metrics.KindUint64 {
		return
	}
	held := int64(min(live[0].Value.Uint64(), math.MaxInt64/2))
	debug.SetMemoryLimit(max(cache.memoryLimit, held+held/2))
}

// queryCache shares immutable query data within one MCP server.
type queryCache struct {
	mu      sync.Mutex // guards entries and loading, never held while a file is decoded
	entries []queryCacheEntry
	// loading holds one lock per directory: its files are decoded by one call
	// at a time, while the directories of a workspace decode side by side.
	loading map[string]*sync.Mutex
	budget  int64 // zero means queryCacheBudget
	// memoryLimit is the soft memory limit tuneMemoryLimit keeps the process
	// under; zero, as in a test or a CLI command, sets none.
	memoryLimit int64
	// memo carries the server's refreshes into each other and hands the graph
	// and ask index a refresh built straight to the next load.
	memo graph.RefreshMemo
}

type queryCacheEntry struct {
	dir                  string
	graphFile, indexFile os.FileInfo
	wiring               *graph.GraphV1
	index                *graph.AskIndex
	indexLoaded          bool
}

// load is safe for concurrent calls; callers must not mutate returned values.
func (cache *queryCache) load(contextDir string) (*graph.GraphV1, *graph.AskIndex, error) {
	return cache.snapshot(contextDir, true)
}

// loadGraph shares the graph without decoding the optional ranked-search index.
func (cache *queryCache) loadGraph(contextDir string) (*graph.GraphV1, error) {
	wiring, _, err := cache.snapshot(contextDir, false)
	return wiring, err
}

func (cache *queryCache) snapshot(contextDir string, withIndex bool) (*graph.GraphV1, *graph.AskIndex, error) {
	if cache == nil {
		wiring, err := graph.Read(graph.WiringPath(contextDir))
		if err != nil {
			return nil, nil, err
		}
		var index *graph.AskIndex
		if withIndex {
			index = graph.ReadAskIndex(filepath.Join(contextDir, ".cache", "ask-index.json"))
		}
		return wiring, index, nil
	}
	dir, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, nil, err
	}
	cache.mu.Lock()
	if cache.loading == nil {
		cache.loading = make(map[string]*sync.Mutex)
	}
	loading := cache.loading[dir]
	if loading == nil {
		loading = new(sync.Mutex)
		cache.loading[dir] = loading
	}
	cache.mu.Unlock()
	loading.Lock()
	defer loading.Unlock()

	graphPath := graph.WiringPath(dir)
	indexPath := filepath.Join(dir, ".cache", "ask-index.json")
	for range 3 {
		graphFile, statErr := os.Stat(graphPath)
		var indexFile os.FileInfo
		if withIndex {
			indexFile, _ = os.Stat(indexPath) // The sidecar is optional, including when unreadable.
		}
		if statErr == nil {
			// What this server's last refresh built needs no decoding, and while
			// its files are being written it is newer than any graph cached here.
			if written, writtenIndex, ok := cache.memo.Written(dir, graphFile, indexFile); ok {
				cache.mu.Lock()
				cache.entries = slices.DeleteFunc(cache.entries, func(entry queryCacheEntry) bool { return entry.dir == dir })
				cache.mu.Unlock()
				return written, writtenIndex, nil
			}
		}
		// A graph that is gone has a nil graphFile, which no entry describes.
		entry, hit := cache.held(dir, graphFile, indexFile, withIndex)
		if hit {
			return entry.wiring, entry.index, nil
		}
		if statErr != nil {
			return nil, nil, statErr
		}
		wiring := entry.wiring
		var readErr error
		if wiring == nil {
			wiring, readErr = graph.Read(graphPath)
		}
		var index *graph.AskIndex
		if withIndex {
			index = graph.ReadAskIndex(indexPath)
		}
		graphAfter, graphStatErr := os.Stat(graphPath)
		var indexAfter os.FileInfo
		if withIndex {
			indexAfter, _ = os.Stat(indexPath) // An absent sidecar must invalidate its cached value.
		}
		if graphStatErr != nil || !sameQueryFile(graphFile, graphAfter) || !sameQueryFile(indexFile, indexAfter) {
			continue
		}
		if readErr != nil {
			return nil, nil, readErr
		}
		cache.keep(queryCacheEntry{dir: dir, graphFile: graphFile, indexFile: indexFile, wiring: wiring, index: index, indexLoaded: withIndex})
		return wiring, index, nil
	}
	return nil, nil, errors.New("query files changed during loading; retry the query")
}

// held takes dir's entry out of the cache and reports a hit when it still
// describes graphFile and, for a load with the index, indexFile; a hit goes
// back as the most recently used entry. A miss returns the graph alone when it
// was cached without its index and is still current, and nothing otherwise.
func (cache *queryCache) held(dir string, graphFile, indexFile os.FileInfo, withIndex bool) (queryCacheEntry, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	i := slices.IndexFunc(cache.entries, func(entry queryCacheEntry) bool { return entry.dir == dir })
	if i < 0 {
		return queryCacheEntry{}, false
	}
	entry := cache.entries[i]
	cache.entries = slices.Delete(cache.entries, i, i+1)
	if !sameQueryFile(entry.graphFile, graphFile) {
		return queryCacheEntry{}, false
	}
	if !withIndex || (entry.indexLoaded && sameQueryFile(entry.indexFile, indexFile)) {
		cache.entries = append(cache.entries, entry)
		return entry, true
	}
	if entry.indexLoaded {
		return queryCacheEntry{}, false
	}
	return queryCacheEntry{wiring: entry.wiring}, false
}

// keep caches entry while the budget holds it. A workspace query loads every
// child, so evicting the least recently used would miss on every child once
// they outgrow the budget; a child that does not fit is decoded per query and
// the cached ones stay.
func (cache *queryCache) keep(entry queryCacheEntry) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	retained := entry.size()
	for _, cached := range cache.entries {
		retained += cached.size()
	}
	if len(cache.entries) == 0 || retained <= cmp.Or(cache.budget, queryCacheBudget) {
		cache.entries = append(cache.entries, entry)
	}
}

// size estimates the entry's heap from its files: a decoded graph takes less
// than its JSON, while an ask index takes about twice its JSON.
func (entry queryCacheEntry) size() int64 {
	size := entry.graphFile.Size()
	if entry.indexFile != nil {
		size += 2 * entry.indexFile.Size()
	}
	return size
}

func sameQueryFile(before, after os.FileInfo) bool {
	if before == nil || after == nil {
		return before == nil && after == nil
	}
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}
