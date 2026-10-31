package hosts

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOpencodePluginTemplate(t *testing.T) {
	plugin := OpencodePlugin()
	for _, want := range []string{
		opencodePluginMarker, `"session.created"`, `"session.idle"`, `"tool.execute.after"`, `ctx.tool.hook("execute.after"`,
		`graftHook("session-start"`, `graftHook("tool-savings"`, `graftHook("stop"`, `graft_host: "opencode"`,
		"experimental.chat.system.transform", "session\\.execution\\.", "export default { id: \"graft\", server, setup }",
	} {
		if !strings.Contains(plugin, want) {
			t.Errorf("OpencodePlugin() lacks %q", want)
		}
	}
	if !strings.HasPrefix(plugin, opencodePluginMarker) {
		t.Errorf("OpencodePlugin() does not start with the ownership marker")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not installed: the template's syntax is not checked")
	}
	path := filepath.Join(t.TempDir(), "graft.ts")
	writeTestFile(t, path, plugin)
	if out, err := exec.CommandContext(t.Context(), bun, "build", "--no-bundle", path).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed program, test file path
		t.Errorf("bun build --no-bundle graft.ts error = %v\n%s", err, out)
	}
}

func TestOpencodePluginWiring(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	plugin := opencodePluginPath(repo)
	if got := OpencodePluginTargets(repo, home); got != nil {
		t.Fatalf("OpencodePluginTargets(no opencode) = %+v, want none", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o750); err != nil {
		t.Fatal(err)
	}
	var planned []string
	for _, write := range SelectedWrites(PlanInit(repo, home, binLaunch, []string{"agents"}), []string{"agents"}) {
		planned = append(planned, write.Path)
	}
	if !slices.Contains(planned, filepath.Join(repo, "opencode.json")) || !slices.Contains(planned, plugin) {
		t.Errorf("PlanInit(agents) paths = %q, want opencode.json and the plugin", planned)
	}

	env := Env{Home: home, Binary: "/opt/graft", Launch: binLaunch}
	if _, err := RunHostsInit(repo, env, InitOptions{Agents: []string{"agents"}, MCP: true}); err != nil {
		t.Fatalf("RunHostsInit(--no-hooks) error = %v", err)
	}
	if _, err := os.Stat(plugin); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("plugin after init --no-hooks: stat error = %v, want not exist", err)
	}
	for i, want := range []WriteAction{ActionCreated, ActionUnchanged} {
		result, err := RunHostsInit(repo, env, InitOptions{Agents: []string{"agents"}, MCP: true, Hooks: true})
		if err != nil {
			t.Fatalf("RunHostsInit() error = %v", err)
		}
		k := slices.IndexFunc(result.Hooks, func(write ConfigWrite) bool { return write.ID == "opencode-plugin" })
		if k < 0 || result.Hooks[k].Action != want {
			t.Errorf("RunHostsInit() run %d plugin write = %+v, want %s", i+1, result.Hooks, want)
		}
	}
	if got := readTestFile(t, plugin); got != OpencodePlugin() {
		t.Errorf("plugin content differs from the template")
	}

	Retract(repo, env, RetractOptions{Apply: true})
	if _, err := os.Stat(filepath.Join(repo, ".opencode")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf(".opencode after uninstall: stat error = %v, want removed", err)
	}
}

func TestOpencodePluginUninstallKeepsForeignFile(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	plugin := opencodePluginPath(repo)
	writeTestFile(t, plugin, "export default {}\n")
	Retract(repo, Env{Home: home, Binary: "/opt/graft", Launch: binLaunch}, RetractOptions{Apply: true})
	if got := readTestFile(t, plugin); got != "export default {}\n" {
		t.Errorf("foreign graft.ts after uninstall = %q, want it kept", got)
	}
}
