package graph

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/h0rn3t/Graft/internal/sourcefiles"
)

// Writing an extraction cache or a fingerprint keeps the newest two of its
// kind in .cache, so the sidecars of older extractor versions do not pile up.

func writeAgedSidecars(t *testing.T, cache string, ages map[string]time.Duration) {
	t.Helper()
	if err := os.MkdirAll(cache, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, age := range ages {
		path := filepath.Join(cache, name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := time.Now().Add(-age)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
}

func cacheFiles(t *testing.T, cache string) []string {
	t.Helper()
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestBuildGraphKeepsTheNewestTwoExtractionCaches(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "graft", ".cache")
	writeAgedSidecars(t, cache, map[string]time.Duration{
		"extract.go-v1.json": 3 * time.Hour,
		"extract.go-v2.json": 2 * time.Hour,
		"extract.go-v3.json": time.Hour,
		// Not extraction caches, whatever their age.
		"fingerprint.go-v1.json": 3 * time.Hour,
		"ask-index.json":         3 * time.Hour,
		"extract.notes.txt":      3 * time.Hour,
	})

	if _, err := BuildGraph(root, sourcefiles.Options{OutDir: "graft"}); err != nil {
		t.Fatalf("BuildGraph(%q) error = %v, want nil", root, err)
	}

	want := []string{"ask-index.json", "extract." + ExtractorID + ".json", "extract.go-v3.json", "extract.notes.txt", "fingerprint.go-v1.json"}
	if got := cacheFiles(t, cache); !slices.Equal(got, want) {
		t.Errorf("after BuildGraph, .cache holds %v, want %v", got, want)
	}
}

func TestBuildGraphWithoutCacheWriteRemovesNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "graft", ".cache")
	writeAgedSidecars(t, cache, map[string]time.Duration{
		"extract.go-v1.json": 3 * time.Hour,
		"extract.go-v2.json": 2 * time.Hour,
		"extract.go-v3.json": time.Hour,
	})

	// A read-only check of the graph writes nothing, so it removes nothing.
	if _, err := BuildGraph(root, sourcefiles.Options{OutDir: "graft", NoCacheWrite: true}); err != nil {
		t.Fatalf("BuildGraph(%q, NoCacheWrite) error = %v, want nil", root, err)
	}

	want := []string{"extract.go-v1.json", "extract.go-v2.json", "extract.go-v3.json"}
	if got := cacheFiles(t, cache); !slices.Equal(got, want) {
		t.Errorf("after BuildGraph with NoCacheWrite, .cache holds %v, want %v", got, want)
	}
}

func TestWriteFingerprintKeepsTheNewestTwoFingerprints(t *testing.T) {
	outDir := t.TempDir()
	cache := filepath.Join(outDir, ".cache")
	writeAgedSidecars(t, cache, map[string]time.Duration{
		"fingerprint.go-v1.json": 3 * time.Hour,
		"fingerprint.go-v2.json": 2 * time.Hour,
		"fingerprint.go-v3.json": time.Hour,
		"extract.go-v1.json":     3 * time.Hour,
	})

	if err := WriteFingerprint(outDir, ExtractorID, nil, nil); err != nil {
		t.Fatalf("WriteFingerprint(%q) error = %v, want nil", outDir, err)
	}

	want := []string{"extract.go-v1.json", "fingerprint." + ExtractorID + ".json", "fingerprint.go-v3.json"}
	if got := cacheFiles(t, cache); !slices.Equal(got, want) {
		t.Errorf("after WriteFingerprint, .cache holds %v, want %v", got, want)
	}
}
