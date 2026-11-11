package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/h0rn3t/Graft/internal/graph"
)

// graphPaths lists the files the graph of root holds a node for.
func graphPaths(t *testing.T, root string) []string {
	t.Helper()
	loaded, err := graph.Read(graph.WiringPath(filepath.Join(root, "graft")))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, node := range loaded.Nodes {
		if node.Kind == "file" {
			paths = append(paths, node.Path)
		}
	}
	slices.Sort(paths)
	return paths
}

func TestBuildExcludeDirLeavesADirectoryOutUntilTheConfigChanges(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"src/app.ts", "src/fixtures/copy.ts", "src/fixtures2/keep.ts"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("export function mark() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"src/app.ts", "src/fixtures2/keep.ts"}

	var stdout, stderr bytes.Buffer
	args := []string{"build", root, "--exclude-dir", "./src/fixtures/"}
	if status := run(args, &stdout, &stderr); status != 0 {
		t.Fatalf("run(%v) status = %d, want 0; stderr = %q", args, status, stderr.String())
	}
	if got := graphPaths(t, root); !slices.Equal(got, want) {
		t.Errorf("run(%v) graph files = %v, want %v", args, got, want)
	}
	config, err := os.ReadFile(filepath.Join(root, ".graft", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"excludeDirs\": [\n    \"src/fixtures\"\n  ]\n}"; string(config) != want {
		t.Errorf("run(%v) config = %q, want %q", args, config, want)
	}

	// The choice is persisted: a later build without the flag walks the same set.
	stdout.Reset()
	stderr.Reset()
	args = []string{"build", root, "--no-reuse"}
	if status := run(args, &stdout, &stderr); status != 0 {
		t.Fatalf("run(%v) status = %d, want 0; stderr = %q", args, status, stderr.String())
	}
	if got := graphPaths(t, root); !slices.Equal(got, want) {
		t.Errorf("run(%v) after a persisted --exclude-dir: graph files = %v, want %v", args, got, want)
	}
}

func TestBuildRejectsInvalidExcludeDirBeforeWritingConfig(t *testing.T) {
	for _, dir := range []string{"", ".", "../sibling", "/abs/path"} {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			var stdout, stderr bytes.Buffer
			args := []string{"build", root, "--exclude-dir", dir}
			if status := run(args, &stdout, &stderr); status == 0 || !strings.Contains(stderr.String(), "--exclude-dir") {
				t.Errorf("run(%v) = status %d, stderr %q; want validation failure", args, status, stderr.String())
			}
			if _, err := os.Stat(filepath.Join(root, ".graft", "config.json")); !os.IsNotExist(err) {
				t.Errorf("run(%v) wrote config, Stat error = %v; want absent", args, err)
			}
		})
	}
}
