package graph

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/h0rn3t/Graft/internal/sourcefiles"
)

func TestRunLSPPassContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake language server is a POSIX shell script")
	}
	// a calls b by name; the language server also resolved a call from a to c
	// that the syntax alone does not show, as through an interface.
	lspEdge := EdgeV1{Source: "a.go#a", Target: "c.go#c", Relation: "calls", Confidence: "lsp_resolved"}
	sources := map[string]string{
		"a.go": "package p\n\nfunc a() { b() }\n",
		"b.go": "package p\n\nfunc b() {}\n",
		"c.go": "package p\n\nfunc c() {}\n",
		"d.go": "package p\n\nfunc d() {}\n",
	}
	ask := LSPPassOptions{Ask: true, Reuse: true}
	tests := []struct {
		name string
		// record is the pass on file before the build: "complete",
		// "unanswered", "elsewhere" (another server's), "older" (another
		// extractor's) or "" for none.
		record  string
		edit    string // the file changed after that pass
		options LSPPassOptions
		// wantStarted reports whether the server is started at all.
		wantStarted bool
		wantKept    bool
		wantSkipped bool
		wantEdge    bool
		// wantRecord is the pass on file after the build: "current" for a
		// complete pass of the current files, "unanswered" for current files
		// the server did not answer for, "before" when it is left as it was,
		// and "" when it is removed.
		wantRecord string
	}{
		{name: "unchanged since a complete pass", record: "complete", options: ask,
			wantKept: true, wantEdge: true, wantRecord: "current"},
		{name: "a change elsewhere keeps the edge", record: "complete", edit: "d.go", options: ask,
			wantStarted: true, wantEdge: true, wantRecord: "unanswered"},
		{name: "a change in a file the caller's last answer named asks the caller again", record: "complete", edit: "c.go", options: ask,
			wantStarted: true, wantRecord: "unanswered"},
		{name: "a change in a file the caller's code names asks the caller again", record: "complete", edit: "b.go", options: ask,
			wantStarted: true, wantRecord: "unanswered"},
		{name: "unanswered for the same files", record: "unanswered", options: ask,
			wantSkipped: true, wantEdge: true, wantRecord: "before"},
		{name: "unanswered and a file changed since", record: "unanswered", edit: "d.go", options: ask,
			wantStarted: true, wantRecord: "unanswered"},
		{name: "no pass on file", options: ask, wantStarted: true, wantRecord: "unanswered"},
		{name: "another server's pass", record: "elsewhere", options: ask, wantStarted: true, wantRecord: "unanswered"},
		{name: "another extractor's pass", record: "older", options: ask, wantStarted: true, wantRecord: "unanswered"},
		{name: "without reuse", record: "complete", options: LSPPassOptions{Ask: true}, wantStarted: true, wantRecord: "unanswered"},
		{name: "not asked, edges of unchanged code are kept", record: "complete", edit: "d.go", options: LSPPassOptions{Reuse: true},
			wantEdge: true, wantRecord: "before"},
		{name: "not asked and no pass on file", edit: "d.go", options: LSPPassOptions{Reuse: true}},
		{name: "not asked without reuse", record: "complete", options: LSPPassOptions{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outDir := filepath.Join(root, "graft")
			for rel, source := range sources {
				writeRefreshSource(t, root, rel, source)
			}
			serverDir := t.TempDir()
			server := filepath.Join(serverDir, "gopls")
			started := filepath.Join(serverDir, "started")
			if err := os.WriteFile(server, []byte("#!/bin/sh\n: > \""+started+"\"\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("WriteFile(%q) error = %v, want nil", server, err)
			}
			t.Setenv("PATH", serverDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			opts := sourcefiles.Options{OutDir: outDir}
			built, err := BuildGraph(root, opts)
			if err != nil {
				t.Fatalf("BuildGraph(%q) error = %v, want nil", root, err)
			}
			enriched := built.Graph
			enriched.Edges = append(slices.Clone(enriched.Edges), lspEdge)
			if _, err := Write(enriched, outDir); err != nil {
				t.Fatalf("Write(graph, %q) error = %v, want nil", outDir, err)
			}
			if tc.record != "" {
				before := &lspPassRecord{Version: lspPassVersion, Extractor: ExtractorID, Server: server, Files: lspFileHashes(built.Fingerprints)}
				switch tc.record {
				case "unanswered":
					before.Unanswered = true
				case "elsewhere":
					before.Server = "/elsewhere/gopls"
				case "older":
					before.Extractor = "go-v0"
				}
				if err := WriteLSPPass(outDir, LSPPass{record: before}); err != nil {
					t.Fatalf("WriteLSPPass(%q) error = %v, want nil", outDir, err)
				}
			}
			recorded := readLSPPassRecord(outDir)
			if tc.edit != "" {
				writeRefreshSource(t, root, tc.edit, sources[tc.edit]+"\n// edited\n")
			}

			rebuilt, err := BuildGraph(root, opts)
			if err != nil {
				t.Fatalf("BuildGraph(%q) error = %v, want nil", root, err)
			}
			pass := RunLSPPass(t.Context(), &rebuilt.Graph, root, outDir, rebuilt.Fingerprints, tc.options)
			if err := WriteLSPPass(outDir, pass); err != nil {
				t.Fatalf("WriteLSPPass(%q) error = %v, want nil", outDir, err)
			}

			if _, err := os.Stat(started); (err == nil) != tc.wantStarted {
				t.Errorf("RunLSPPass(%#v) started the server = %t, want %t", tc.options, err == nil, tc.wantStarted)
			}
			if pass.Kept != tc.wantKept || pass.Skipped != tc.wantSkipped {
				t.Errorf("RunLSPPass(%#v) = (Kept %t, Skipped %t), want (%t, %t)", tc.options, pass.Kept, pass.Skipped, tc.wantKept, tc.wantSkipped)
			}
			if got := slices.Contains(rebuilt.Graph.Edges, lspEdge); got != tc.wantEdge {
				t.Errorf("RunLSPPass(%#v) kept the language-server edge = %t, want %t", tc.options, got, tc.wantEdge)
			}
			if rebuilt.Graph.Meta.EdgeCount != len(rebuilt.Graph.Edges) {
				t.Errorf("RunLSPPass(%#v) meta edge count = %d, want %d", tc.options, rebuilt.Graph.Meta.EdgeCount, len(rebuilt.Graph.Edges))
			}
			after := readLSPPassRecord(outDir)
			current := lspFileHashes(rebuilt.Fingerprints)
			var ok bool
			switch tc.wantRecord {
			case "current":
				ok = after != nil && !after.Unanswered && after.Server == server && maps.Equal(after.Files, current)
			case "unanswered":
				ok = after != nil && after.Unanswered && after.Server == server && maps.Equal(after.Files, current)
			case "before":
				ok = after != nil && recorded != nil && after.Unanswered == recorded.Unanswered && maps.Equal(after.Files, recorded.Files)
			default:
				ok = after == nil
			}
			if !ok {
				t.Errorf("RunLSPPass(%#v) then WriteLSPPass left the record %#v, want %q", tc.options, after, tc.wantRecord)
			}
		})
	}
}

// A refresh carries a caller's language-server edges only while nothing its
// calls resolve through changed. Flipping build constraints edits the files
// that declare the callee, not the caller's own: the edge into b.go must go,
// since the compiler may now resolve Target in c.go.
func TestCarryOverLSPEdgesDropsCallsIntoChangedFiles(t *testing.T) {
	file := func(path, hash string) NodeV1 {
		return NodeV1{ID: path, Name: path, Kind: "file", Path: path, BodyHash: hash}
	}
	function := func(path, name string) NodeV1 {
		return NodeV1{ID: path + "#" + name, Name: name, Kind: "function", Path: path}
	}
	nodes := func(bHash, cHash, dHash string) []NodeV1 {
		return []NodeV1{
			file("a.go", "a1"), function("a.go", "Caller"),
			file("b.go", bHash), function("b.go", "Target"),
			file("c.go", cHash), function("c.go", "Target"),
			file("d.go", dHash), function("d.go", "Other"),
		}
	}
	lspEdge := EdgeV1{Source: "a.go#Caller", Target: "b.go#Target", Relation: "calls", Confidence: "lsp_resolved", Line: 3}
	prior := GraphV1{Nodes: nodes("b1", "c1", "d1"), Edges: []EdgeV1{lspEdge}}
	for _, tt := range []struct {
		name             string
		bHash, cHash     string
		dHash            string
		wantCarriedEdges int
	}{
		{name: "build tags flipped in b.go and c.go", bHash: "b2", cHash: "c2", dHash: "d1", wantCarriedEdges: 0},
		{name: "only the callee file changed", bHash: "b2", cHash: "c1", dHash: "d1", wantCarriedEdges: 0},
		{name: "an unrelated file changed", bHash: "b1", cHash: "c1", dHash: "d2", wantCarriedEdges: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			graph := GraphV1{Nodes: nodes(tt.bHash, tt.cHash, tt.dHash)}
			carryOverLSPEdges(&graph, prior, nil)
			if got := len(graph.Edges); got != tt.wantCarriedEdges {
				t.Errorf("carryOverLSPEdges(b.go %s, c.go %s, d.go %s) edges = %+v, want %d carried", tt.bHash, tt.cHash, tt.dHash, graph.Edges, tt.wantCarriedEdges)
			}
			if graph.Meta.EdgeCount != len(graph.Edges) {
				t.Errorf("carryOverLSPEdges() meta edgeCount = %d, want %d", graph.Meta.EdgeCount, len(graph.Edges))
			}
		})
	}
}
