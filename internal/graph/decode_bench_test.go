package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkRead reads a graph the size of a mid-sized repository's: about
// 6,400 nodes and 27,000 edges, as every query, hook and server start does.
func BenchmarkRead(b *testing.B) {
	path := filepath.Join(b.TempDir(), GraphFile)
	if err := os.WriteFile(path, writtenGraph(b, sampleGraph(376, 27000)), 0o600); err != nil {
		b.Fatalf("os.WriteFile() error = %v, want nil", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Read(path); err != nil {
			b.Fatalf("Read() error = %v, want nil", err)
		}
	}
}
