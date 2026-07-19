package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

func newOp(factory dag.Factory, name string, oc map[string]any) interface {
	Process(map[string]any) (map[string]any, error)
} {
	return factory.New(name, oc)
}

func setTargetConf(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ovcli.conf")
	if err := os.WriteFile(path, []byte(`{"url":"http://ov.local","api_key":"conf-key","account":"acct","user":"user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)
}

func TestClaudeExecArgvEnvAndReply(t *testing.T) {
	setTargetConf(t)
	cwd := filepath.Join(t.TempDir(), "claude-cwd")
	stateDir := filepath.Join(t.TempDir(), "claude-state")
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(`{"name":"openviking-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_BIN", "/tmp/claude-test")

	var seen []string
	var env map[string]string
	var timeout int
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, envExtra map[string]string, _, timeoutSec int) root.CLIResult {
		seen = append([]string(nil), argv...)
		env = envExtra
		timeout = timeoutSec
		return root.CLIResult{
			ExitCode: 0,
			Stdout: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"text","text":"ACK from assistant"}]}}`,
				`{"type":"result","subtype":"success","result":"ACK from result","terminal_reason":"completed"}`,
			}, "\n") + "\n",
		}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Exec, "chat", map[string]any{
		"message":               "use OpenViking",
		"cwd":                   cwd,
		"state_dir":             stateDir,
		"timeout":               41,
		"openviking_endpoint":   "http://cfg.ov",
		"openviking_api_key":    "cfg-key",
		"openviking_account":    "acct-1",
		"openviking_user":       "user-1",
		"openviking_peer_id":    "peer-1",
		"plugin_root":           pluginRoot,
		"auto_capture":          false,
		"auto_recall":           true,
		"no_auto_inject":        true,
		"bypass_permissions":    true,
		"disable_builtin_tools": true,
		"model":                 "test-model",
		"session_id":            "11111111-1111-4111-8111-111111111111",
	}).Process(map[string]any{"user_key": "wired-key"})
	if err != nil {
		t.Fatalf("claude exec should pass: %v", err)
	}
	canonicalPluginRoot, err := filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{
		"/tmp/claude-test",
		"-p",
		"--plugin-dir", canonicalPluginRoot,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
		"--tools", "",
		"--debug-file", filepath.Join(stateDir, "chat-debug.log"),
		"--model", "test-model",
		"--session-id", "11111111-1111-4111-8111-111111111111",
		"use OpenViking",
	}
	if strings.Join(seen, "\n") != strings.Join(wantArgv, "\n") {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
	if timeout != 41 {
		t.Fatalf("timeout = %d, want 41", timeout)
	}
	for key, want := range map[string]string{
		"OPENVIKING_CREDENTIAL_SOURCE": "cli",
		"OPENVIKING_CLI_CONFIG_FILE":   filepath.Join(stateDir, "ovcli.conf"),
		"OPENVIKING_URL":               "http://cfg.ov",
		"OPENVIKING_API_KEY":           "cfg-key",
		"OPENVIKING_ACCOUNT":           "acct-1",
		"OPENVIKING_USER":              "user-1",
		"OPENVIKING_PEER_ID":           "peer-1",
		"OPENVIKING_MEMORY_ENABLED":    "1",
		"OPENVIKING_AUTO_CAPTURE":      "0",
		"OPENVIKING_AUTO_RECALL":       "1",
		"OPENVIKING_NO_AUTO_INJECT":    "1",
		"OPENVIKING_WRITE_PATH_ASYNC":  "0",
		"OPENVIKING_DEBUG":             "1",
		"NO_COLOR":                     "1",
	} {
		if env[key] != want {
			t.Fatalf("env[%s] = %q, want %q; env=%v", key, env[key], want, env)
		}
	}
	rawConf, err := os.ReadFile(env["OPENVIKING_CLI_CONFIG_FILE"])
	if err != nil {
		t.Fatalf("read generated ovcli config: %v", err)
	}
	var cliConf map[string]any
	if err := json.Unmarshal(rawConf, &cliConf); err != nil {
		t.Fatalf("decode generated ovcli config: %v", err)
	}
	if cliConf["url"] != "http://cfg.ov" || cliConf["api_key"] != "cfg-key" || cliConf["account"] != "acct-1" || cliConf["user"] != "user-1" || cliConf["actor_peer_id"] != "peer-1" {
		t.Fatalf("generated ovcli config = %v", cliConf)
	}
	if out["reply"] != "ACK from result" || out["jsonl"] == "" ||
		out["ov_session_id"] != "cc-11111111-1111-4111-8111-111111111111" ||
		out["jsonl_path"] != filepath.Join(cwd, "chat.jsonl") ||
		out["state_dir"] != stateDir ||
		out["cli_config_path"] != env["OPENVIKING_CLI_CONFIG_FILE"] {
		t.Fatalf("out = %v", out)
	}
}

func TestClaudeOVSessionIDsUseStructuredLifecycleEvents(t *testing.T) {
	parent, child := claudeOVSessionIDs(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"parent-123"}`,
		`{"type":"system","subtype":"task_started","session_id":"parent-123","task_id":"child:456"}`,
	}, "\n"), "")
	if parent != "cc-parent-123" || child != "cc-parent-123__subagent-child-456" {
		t.Fatalf("session IDs = %q / %q", parent, child)
	}
}

func TestClaudeExecUsesExplicitIsolatedSettings(t *testing.T) {
	setTargetConf(t)
	t.Setenv("OV_TEST_CLAUDE_BIN", "/tmp/claude-test")
	settings := filepath.Join(t.TempDir(), "auth-settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_SETTINGS", settings)
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(`{"name":"openviking-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_PLUGIN_ROOT", pluginRoot)
	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"result","result":"ACK"}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()
	_, err := newOp(Exec, "chat", map[string]any{
		"message": "test", "cwd": t.TempDir(), "state_dir": t.TempDir(), "setting_sources": "",
	}).Process(map[string]any{"user_key": "wired-key"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, " ")
	if !strings.Contains(joined, "--settings "+settings) || !strings.Contains(joined, "--setting-sources ") {
		t.Fatalf("argv did not isolate settings/auth: %v", seen)
	}
}

func TestClaudeExecUsesStrictSecretMCPConfig(t *testing.T) {
	setTargetConf(t)
	t.Setenv("OV_TEST_CLAUDE_BIN", "/tmp/claude-test")
	t.Setenv("OV_TEST_CLAUDE_AUTH_HOME", t.TempDir())
	secretDir := filepath.Join(t.TempDir(), "always-deleted")
	t.Setenv("OV_TEST_SECRET_STATE_DIR", secretDir)
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginRoot, "servers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(`{"name":"openviking-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "servers", "mcp-proxy.mjs"), []byte("// test"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"result","result":"ACK"}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Exec, "tools", map[string]any{
		"message": "test", "cwd": t.TempDir(), "state_dir": t.TempDir(),
		"plugin_root": pluginRoot, "isolated_mcp": true,
		"openviking_url": "http://ov.local", "openviking_api_key": "mcp-secret",
	}).Process(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, " ")
	if !strings.Contains(joined, "--strict-mcp-config") || strings.Contains(joined, "mcp-secret") {
		t.Fatalf("isolated MCP argv = %v", seen)
	}
	var configPath string
	for i := range seen {
		if seen[i] == "--mcp-config" && i+1 < len(seen) {
			configPath = seen[i+1]
		}
	}
	if !strings.HasPrefix(configPath, secretDir+string(filepath.Separator)) {
		t.Fatalf("MCP config path %q is not below secret root %q", configPath, secretDir)
	}
	if raw, readErr := os.ReadFile(configPath); readErr != nil || !strings.Contains(string(raw), "mcp-secret") {
		t.Fatalf("secret MCP config = %q, err = %v", raw, readErr)
	}
}

func TestClaudeEnvironmentKeepsOpenVikingCredentialsInSecretState(t *testing.T) {
	setTargetConf(t)
	stateDir := filepath.Join(t.TempDir(), "retained-runtime")
	secretDir := filepath.Join(t.TempDir(), "always-deleted")
	authHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(authHome, ".claude.json"), []byte(`{"oauth":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_AUTH_HOME", authHome)
	t.Setenv("OV_TEST_SECRET_STATE_DIR", secretDir)
	queueKey := filepath.Join(secretDir, "environment-queue.key")
	t.Setenv("OPENVIKING_QUEUE_SCOPE_KEY_FILE", queueKey)

	env, _, cliConfigPath, _, err := claudeOpenVikingEnv("chat", map[string]any{
		"state_dir": stateDir, "openviking_url": "http://ov.local",
		"recall_peer_scope": "actor",
	}, "wired-key", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cliConfigPath, secretDir+string(filepath.Separator)) {
		t.Fatalf("CLI config %q is not below secret root %q", cliConfigPath, secretDir)
	}
	isolatedHome := env["HOME"]
	if !strings.HasPrefix(isolatedHome, secretDir+string(filepath.Separator)) {
		t.Fatalf("HOME %q is not below secret root %q", isolatedHome, secretDir)
	}
	if raw, err := os.ReadFile(filepath.Join(isolatedHome, ".claude.json")); err != nil || !strings.Contains(string(raw), `"oauth"`) {
		t.Fatalf("isolated Claude auth = %q, err = %v", raw, err)
	}
	if got := env["CLAUDE_CONFIG_DIR"]; got != filepath.Join(isolatedHome, ".claude") {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", got)
	}
	if got := env["OPENVIKING_QUEUE_SCOPE_KEY_FILE"]; got != queueKey {
		t.Fatalf("queue scope key = %q, want %q", got, queueKey)
	}
	if got := env["OPENVIKING_RECALL_PEER_SCOPE"]; got != "actor" {
		t.Fatalf("recall peer scope = %q, want actor", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ovcli.conf")); !os.IsNotExist(err) {
		t.Fatalf("OpenViking credentials must not be retained in runtime, stat error = %v", err)
	}
}

func TestWriteClaudeMCPConfigIsExplicitAndSecret(t *testing.T) {
	secretDir := t.TempDir()
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, "servers"), 0o700); err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(pluginRoot, "servers", "mcp-proxy.mjs")
	if err := os.WriteFile(proxyPath, []byte("// test"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := writeClaudeMCPConfig("tools", map[string]any{}, secretDir, pluginRoot, map[string]string{
		"HOME": "/isolated/home", "OPENVIKING_URL": "http://ov.local", "OPENVIKING_API_KEY": "secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("MCP config mode = %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"mcpServers"`, `"openviking"`, proxyPath, `"secret-key"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("MCP config missing %q: %s", want, raw)
		}
	}
	disabledPath, err := writeClaudeMCPConfig("hooks", map[string]any{"disable_mcp": true}, secretDir, pluginRoot, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := os.ReadFile(disabledPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disabled), `"openviking"`) || !strings.Contains(string(disabled), `"mcpServers": {}`) {
		t.Fatalf("disabled MCP config = %s", disabled)
	}
}

func TestClaudeExecRendersMessageTemplate(t *testing.T) {
	setTargetConf(t)
	cwd := filepath.Join(t.TempDir(), "claude-cwd")
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(`{"name":"openviking-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_BIN", "/tmp/claude-test")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"result","result":"OK"}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Exec, "chat", map[string]any{
		"message_template": "ingest {{resource_url}} for {{case_id}}",
		"cwd":              cwd,
		"plugin_root":      pluginRoot,
	}).Process(map[string]any{"resource_url": "https://example.test/r.md", "case_id": "claude-case"})
	if err != nil {
		t.Fatalf("claude exec should pass: %v", err)
	}
	if got := seen[len(seen)-1]; got != "ingest https://example.test/r.md for claude-case" {
		t.Fatalf("rendered prompt = %q", got)
	}
}

func TestClaudeExecDefaultDirsAreRunIsolated(t *testing.T) {
	setTargetConf(t)
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(`{"name":"openviking-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_BIN", "/tmp/claude-test")

	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"result","result":"OK"}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	out1, err := newOp(Exec, "chat", map[string]any{"message": "one", "plugin_root": pluginRoot}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("first claude exec should pass: %v", err)
	}
	out2, err := newOp(Exec, "chat", map[string]any{"message": "two", "plugin_root": pluginRoot}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("second claude exec should pass: %v", err)
	}
	if out1["cwd"] == out2["cwd"] || out1["state_dir"] == out2["state_dir"] {
		t.Fatalf("default dirs must be run-isolated, got cwd/state %q/%q then %q/%q",
			out1["cwd"], out1["state_dir"], out2["cwd"], out2["state_dir"])
	}
}

func TestClaudeEvidenceRequiresActualMCPToolEvents(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_health","name":"mcp__plugin_openviking-memory_openviking__health"}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"call_health","type":"tool_result","content":"{\"result\":\"ok\"}"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_remember","name":"mcp__plugin_openviking-memory_openviking__remember"}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"call_remember","type":"tool_result","content":"{\"result\":\"ok\"}"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_search","name":"mcp__plugin_openviking-memory_openviking__search"}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"call_search","type":"tool_result","content":"{\"result\":\"ok\"}"}]}}`,
	}, "\n")

	out, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health", "remember", "search"},
		"expect":       []string{"ok"},
	}).Process(map[string]any{"jsonl": jsonl, "reply": "done"})
	if err != nil {
		t.Fatalf("expected tool evidence should pass: %v", err)
	}
	if out["ok"] != true || !strings.Contains(out["text"].(string), "ok") {
		t.Fatalf("out = %v", out)
	}
}

func TestClaudeEvidenceRejectsPromptOnlyToolNames(t *testing.T) {
	jsonl := `{"type":"assistant","message":{"content":[{"type":"text","name":"mcp__plugin_openviking-memory_openviking__health","text":"not a tool use"}]}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("prompt-only tool names must not satisfy evidence, got %v", err)
	}
}

func TestClaudeEvidenceRejectsToolUseWithoutResult(t *testing.T) {
	jsonl := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_health","name":"mcp__plugin_openviking-memory_openviking__health"}]}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("tool use without result must not satisfy evidence, got %v", err)
	}
}

func TestClaudeEvidenceRejectsMissingTool(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_health","name":"mcp__plugin_openviking-memory_openviking__health"}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"call_health","type":"tool_result","content":"{\"result\":\"ok\"}"}]}}`,
	}, "\n")

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health", "remember"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "remember") {
		t.Fatalf("missing tool must be a gate fail with tool detail, got %v", err)
	}
}

func TestClaudeEvidenceRejectsBusinessErrorResult(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_health","name":"mcp__plugin_openviking-memory_openviking__health"}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"call_health","type":"tool_result","content":"Error: unauthorized"}]}}`,
	}, "\n")
	_, err := newOp(Evidence, "evidence", map[string]any{"expect_tools": []string{"health"}}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("business-error result must not satisfy evidence, got %v", err)
	}
}

func TestClaudeEvidenceForbidsAttemptedTool(t *testing.T) {
	jsonl := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_search","name":"mcp__plugin_openviking-memory_openviking__search"}]}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"forbid_tools": []string{"search"}}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "search") {
		t.Fatalf("attempted forbidden tool must fail, got %v", err)
	}
}

func TestClaudeEvidenceForbidsAnyTool(t *testing.T) {
	jsonl := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call_read","name":"Read","input":{"file_path":"/tmp/memory.md"}}]}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"forbid_any_tool": true}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(strings.ToLower(gf.Detail), "read") {
		t.Fatalf("attempted non-OpenViking tool must fail a tool-free gate, got %v", err)
	}
}

func TestClaudeEvidenceRequiresStructuredHookEvents(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"system","subtype":"hook_started","hook_event":"SubagentStart"}`,
		`{"type":"system","subtype":"hook_response","hook_event":"SubagentStop"}`,
	}, "\n")
	if _, err := newOp(Evidence, "hooks", map[string]any{"expect_hooks": []string{"SubagentStart", "SubagentStop"}}).Process(map[string]any{"jsonl": jsonl}); err != nil {
		t.Fatalf("structured hook evidence should pass: %v", err)
	}
	_, err := newOp(Evidence, "hooks", map[string]any{"expect_hooks": []string{"SubagentStart", "SubagentStop"}}).Process(map[string]any{"jsonl": `{"type":"text","text":"SubagentStart SubagentStop"}`})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "SubagentStart") {
		t.Fatalf("hook names in text must not satisfy evidence, got %v", err)
	}
}
