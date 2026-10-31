package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// builtMCPRepo returns a repository with a built graph.
func builtMCPRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GRAFT_DIR", "")
	t.Setenv("GRAFT_NO_REFRESH", "1")
	root := t.TempDir()
	writeFixtureFile(t, root, "main.go", "package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")
	if status := runBuild(callersOptions{command: "build", root: root, rootSet: true}, io.Discard, io.Discard); status != 0 {
		t.Fatalf("runBuild(%q) status = %d, want 0", root, status)
	}
	return root
}

// seedToolRecord records ten sessions that called every catalog tool but
// graft_check_freshness, with starts MCP starts already counted.
func seedToolRecord(t *testing.T, root string, starts int) {
	t.Helper()
	var used []string
	for _, definition := range mcpTools {
		if definition.Name != "graft_check_freshness" {
			used = append(used, definition.Name)
		}
	}
	err := updatePushLedger(root, hookHostClaude, func(ledger *pushLedger) bool {
		ledger.Tools = &toolRecord{Sessions: toolSessions(toolAuditSessions, used...), Starts: starts}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mcpSession starts one MCP server as client, sends requests after
// initialize, and returns each response's result by id.
func mcpSession(t *testing.T, root, client string, requests ...string) map[float64]map[string]any {
	t.Helper()
	lines := []string{fmt.Sprintf(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":%q,"version":"1"}}}`, client)}
	for i, request := range requests {
		lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,%s}`, i+1, request))
	}
	var stdout, stderr bytes.Buffer
	if status := runMCP(t.Context(), callersOptions{command: "mcp", root: root, rootSet: true}, strings.NewReader(strings.Join(lines, "\n")+"\n"), &stdout, &stderr); status != 0 {
		t.Fatalf("runMCP() status = %d, stderr = %q", status, stderr.String())
	}
	results := make(map[float64]map[string]any)
	for line := range strings.Lines(stdout.String()) {
		var response struct {
			ID     float64        `json:"id"`
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal([]byte(line), &response) == nil {
			results[response.ID] = response.Result
		}
	}
	return results
}

func listedTools(result map[string]any) []string {
	tools, _ := result["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool.(map[string]any)["name"].(string)
		names = append(names, name)
	}
	return names
}

const toolsListRequest = `"method":"tools/list"`

func TestMCPToolsListHidesUnusedTools(t *testing.T) {
	for _, tc := range []struct {
		name       string
		client     string
		starts     int
		corrupt    bool
		wantHidden bool
	}{
		{"claude code", "claude-code", 0, false, true},
		{"unidentified client", "codex-mcp-client", 0, false, false},
		{"fifth start re-audits", "claude-code", pushReauditEvery - 1, false, false},
		{"sixth start hides again", "claude-code", pushReauditEvery, false, true},
		{"corrupt ledger", "claude-code", 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := builtMCPRepo(t)
			seedToolRecord(t, root, tc.starts)
			if tc.corrupt {
				if err := os.WriteFile(pushLedgerPath(root, hookHostClaude), []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			results := mcpSession(t, root, tc.client, toolsListRequest, toolsListRequest)
			first, second := listedTools(results[1]), listedTools(results[2])
			if hidden := !slices.Contains(first, "graft_check_freshness"); hidden != tc.wantHidden || len(first) < len(mcpTools)-1 {
				t.Errorf("tools/list for %s = %v, want graft_check_freshness hidden = %v and every other tool", tc.client, first, tc.wantHidden)
			}
			if !slices.Equal(first, second) {
				t.Errorf("second tools/list = %v, want the first %v", second, first)
			}
		})
	}
}

func TestMCPToolsListCountsStartsWhileHiding(t *testing.T) {
	root := builtMCPRepo(t)
	seedToolRecord(t, root, 0)
	mcpSession(t, root, "claude-code", toolsListRequest)
	mcpSession(t, root, "claude-code", toolsListRequest)
	if got := readPushLedger(root, hookHostClaude).Tools.Starts; got != 2 {
		t.Errorf("starts after two hiding starts = %d, want 2", got)
	}
	err := updatePushLedger(root, hookHostClaude, func(ledger *pushLedger) bool {
		ledger.recordTools("s0", []string{"graft_check_freshness"})
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := listedTools(mcpSession(t, root, "claude-code", toolsListRequest)[1]); len(got) != len(mcpTools) {
		t.Errorf("tools/list after a recorded use = %v, want every tool", got)
	}
	if got := readPushLedger(root, hookHostClaude).Tools.Starts; got != 0 {
		t.Errorf("starts once nothing is hidden = %d, want 0", got)
	}
}

func TestMCPHiddenToolStillAnswers(t *testing.T) {
	root := builtMCPRepo(t)
	call := `"method":"tools/call","params":{"name":"graft_check_freshness","arguments":{}}`
	want := mcpSession(t, root, "claude-code", call)[1]
	seedToolRecord(t, root, 0)
	results := mcpSession(t, root, "claude-code", toolsListRequest, call)
	if slices.Contains(listedTools(results[1]), "graft_check_freshness") {
		t.Fatalf("tools/list = %v, want graft_check_freshness hidden", listedTools(results[1]))
	}
	if got := results[2]; fmt.Sprint(got) != fmt.Sprint(want) || got["isError"] == true {
		t.Errorf("tools/call graft_check_freshness while hidden = %v, want %v", got, want)
	}
}
