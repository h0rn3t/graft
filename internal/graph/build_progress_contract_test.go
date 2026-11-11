package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/h0rn3t/Graft/internal/sourcefiles"
)

// Files are read and parsed on several goroutines, yet OnProgress hears of
// each file once, in walk order, and never from two goroutines at a time.

func TestBuildGraphReportsProgressInWalkOrder(t *testing.T) {
	root := t.TempDir()
	const count = 64
	want := make([]string, 0, count)
	for i := range count {
		rel := fmt.Sprintf("pkg%02d/file.go", i)
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		// Sizes differ so the files do not finish in the order they start.
		source := fmt.Sprintf("package pkg%02d\n\nfunc Run() {}\n%s", i, strings.Repeat("\nfunc pad() {}\n", (i*37)%50))
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		want = append(want, fmt.Sprintf("%d/%d %s", i, count, rel))
	}

	for _, run := range []string{"cold", "from the cache"} {
		// Unsynchronized on purpose: the race detector fails overlapping calls.
		var got []string
		built, err := BuildGraph(root, sourcefiles.Options{
			OutDir: "graft",
			OnProgress: func(index, total int, file string) {
				got = append(got, fmt.Sprintf("%d/%d %s", index, total, file))
			},
		})
		if err != nil {
			t.Fatalf("BuildGraph(%q) %s error = %v, want nil", root, run, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("BuildGraph(%q) %s reported progress\n%v\nwant\n%v", root, run, got, want)
		}
		if built.Parsed+built.Reused != count {
			t.Errorf("BuildGraph(%q) %s parsed %d and reused %d files, want %d in all", root, run, built.Parsed, built.Reused, count)
		}
	}
}
