package opencode

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
	authPath := filepath.Join(dir, "opencode-auth.json")
	if err := os.WriteFile(authPath, []byte(`{"test":{"type":"api","key":"redacted-test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_OPENCODE_AUTH_FILE", authPath)
}

func makePluginRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "plugin")
	for _, dir := range []string{"lib", "servers", "wrappers", "tests"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		"index.mjs":                "export default function OpenVikingPlugin() { return {} }\n",
		"package.json":             `{"type":"module"}` + "\n",
		"lib/config.mjs":           "export {}\n",
		"servers/mcp-proxy.mjs":    "export {}\n",
		"wrappers/openviking.js":   `export { default } from "./openviking/index.mjs"` + "\n",
		"README.md":                "test plugin\n",
		"tests/not-copied.test.js": "skip\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestOpenCodeEnvironmentKeepsAuthInSecretState(t *testing.T) {
	setTargetConf(t)
	stateDir := filepath.Join(t.TempDir(), "retained-runtime")
	secretDir := filepath.Join(t.TempDir(), "always-deleted")
	t.Setenv("OV_TEST_SECRET_STATE_DIR", secretDir)
	queueKey := filepath.Join(secretDir, "environment-queue.key")
	t.Setenv("OPENVIKING_QUEUE_SCOPE_KEY_FILE", queueKey)

	env, _, cliConfigPath, _, configDir, _, err := opencodeOpenVikingEnv("chat", map[string]any{
		"state_dir": stateDir,
	}, "wired-key", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dataHome := env["XDG_DATA_HOME"]
	if !strings.HasPrefix(dataHome, secretDir+string(filepath.Separator)) {
		t.Fatalf("XDG_DATA_HOME %q is not below secret root %q", dataHome, secretDir)
	}
	if got := env["HOME"]; got != filepath.Join(stateDir, "home") {
		t.Fatalf("isolated HOME = %q", got)
	}
	if got := env["OPENVIKING_QUEUE_SCOPE_KEY_FILE"]; got != queueKey {
		t.Fatalf("queue scope key = %q, want %q", got, queueKey)
	}
	for name, path := range map[string]string{"CLI config": cliConfigPath, "OpenCode config": configDir} {
		if !strings.HasPrefix(path, secretDir+string(filepath.Separator)) {
			t.Fatalf("%s %q is not below secret root %q", name, path, secretDir)
		}
	}
	if _, err := os.Stat(filepath.Join(dataHome, "opencode", "auth.json")); err != nil {
		t.Fatalf("secret auth copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "xdg-data", "opencode", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth must not be retained in runtime, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ovcli.conf")); !os.IsNotExist(err) {
		t.Fatalf("OpenViking credentials must not be retained in runtime, stat error = %v", err)
	}
}

func TestOpenCodeExecArgvEnvPluginInstallAndReply(t *testing.T) {
	setTargetConf(t)
	t.Setenv("XDG_CONFIG_HOME", "/ambient/opencode-config")
	pluginRoot := makePluginRoot(t)
	projectDir := filepath.Join(t.TempDir(), "project")
	stateDir := filepath.Join(t.TempDir(), "opencode-state")
	t.Setenv("OV_TEST_OPENCODE_BIN", "/tmp/opencode-test")

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
				`{"type":"step_start","sessionID":"ses_1","part":{"type":"step-start"}}`,
				`{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"ACK from opencode","time":{"end":1}}}`,
			}, "\n") + "\n",
		}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Exec, "chat", map[string]any{
		"message":             "use OpenViking",
		"project_dir":         projectDir,
		"state_dir":           stateDir,
		"timeout":             41,
		"openviking_endpoint": "http://cfg.ov",
		"openviking_api_key":  "cfg-key",
		"openviking_account":  "acct-1",
		"openviking_user":     "user-1",
		"openviking_peer_id":  "peer-1",
		"plugin_root":         pluginRoot,
		"model":               "test/model",
		"auto_capture":        false,
		"auto_recall":         true,
		"score_threshold":     "0.12",
	}).Process(map[string]any{"user_key": "wired-key"})
	if err != nil {
		t.Fatalf("opencode exec should pass: %v", err)
	}
	wantArgv := []string{
		"/tmp/opencode-test", "run",
		"--auto",
		"--format", "json",
		"--dir", projectDir,
		"--model", "test/model",
		"use OpenViking",
	}
	if !equalStrs(seen, wantArgv) {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
	if strings.Contains(strings.Join(seen, " "), "--pure") {
		t.Fatalf("opencode adapter must not pass --pure: %v", seen)
	}
	if timeout != 41 {
		t.Fatalf("timeout = %d, want 41", timeout)
	}
	if out["ov_session_id"] != "oc-ses_1" {
		t.Fatalf("ov_session_id = %v", out["ov_session_id"])
	}
	for _, key := range []string{
		"OPENCODE_TEST_HOME",
		"OPENCODE_CONFIG_DIR",
		"XDG_CONFIG_HOME",
		"OPENVIKING_CONFIG_FILE",
		"OPENVIKING_CLI_CONFIG_FILE",
		"OPENVIKING_PLUGIN_CONFIG",
		"OPENVIKING_CREDENTIAL_SOURCE",
		"OPENVIKING_DEBUG_LOG",
		"OPENVIKING_PENDING_DIR",
	} {
		if env[key] == "" {
			t.Fatalf("env missing %s: %v", key, env)
		}
	}
	if env["OPENVIKING_URL"] != "http://cfg.ov" ||
		env["OPENVIKING_API_KEY"] != "cfg-key" ||
		env["OPENVIKING_ACCOUNT"] != "acct-1" ||
		env["OPENVIKING_USER"] != "user-1" ||
		env["OPENVIKING_PEER_ID"] != "peer-1" ||
		env["OPENVIKING_CREDENTIAL_SOURCE"] != "cli" ||
		env["OPENVIKING_AUTO_CAPTURE"] != "0" ||
		env["OPENVIKING_AUTO_RECALL"] != "1" ||
		env["OPENVIKING_SCORE_THRESHOLD"] != "0.12" {
		t.Fatalf("env = %v", env)
	}
	if got := env["OPENCODE_CONFIG_DIR"]; !strings.HasPrefix(got, stateDir) {
		t.Fatalf("OPENCODE_CONFIG_DIR = %q, want under %q", got, stateDir)
	}
	if got := env["XDG_CONFIG_HOME"]; got == "/ambient/opencode-config" || got == "" {
		t.Fatalf("XDG_CONFIG_HOME was not isolated: %q", got)
	}
	if raw, err := os.ReadFile(filepath.Join(env["XDG_DATA_HOME"], "opencode", "auth.json")); err != nil || !strings.Contains(string(raw), `"redacted-test"`) {
		t.Fatalf("isolated OpenCode auth copy: raw=%q err=%v", string(raw), err)
	}
	wrapper := filepath.Join(env["OPENCODE_CONFIG_DIR"], "plugins", "openviking.js")
	pkgIndex := filepath.Join(env["OPENCODE_CONFIG_DIR"], "plugins", "openviking", "index.mjs")
	if _, err := os.Stat(wrapper); err != nil {
		t.Fatalf("installed plugin wrapper: %v", err)
	}
	if _, err := os.Stat(pkgIndex); err != nil {
		t.Fatalf("installed plugin package index: %v", err)
	}
	rawConf, err := os.ReadFile(env["OPENVIKING_CLI_CONFIG_FILE"])
	if err != nil {
		t.Fatalf("read generated ovcli config: %v", err)
	}
	var cliConf map[string]any
	if err := json.Unmarshal(rawConf, &cliConf); err != nil {
		t.Fatalf("decode generated ovcli config: %v", err)
	}
	if cliConf["url"] != "http://cfg.ov" || cliConf["api_key"] != "cfg-key" ||
		cliConf["account"] != "acct-1" || cliConf["user"] != "user-1" ||
		cliConf["actor_peer_id"] != "peer-1" {
		t.Fatalf("generated ovcli config = %v", cliConf)
	}
	if out["reply"] != "ACK from opencode" || out["jsonl"] == "" ||
		out["jsonl_path"] != filepath.Join(projectDir, "chat.jsonl") ||
		out["project_dir"] != projectDir ||
		out["state_dir"] != stateDir ||
		out["cli_config_path"] != env["OPENVIKING_CLI_CONFIG_FILE"] ||
		out["plugin_config_path"] != env["OPENVIKING_PLUGIN_CONFIG"] ||
		out["opencode_config_dir"] != env["OPENCODE_CONFIG_DIR"] {
		t.Fatalf("out = %v", out)
	}
}

func TestOpenCodeExecRendersMessageTemplate(t *testing.T) {
	setTargetConf(t)
	pluginRoot := makePluginRoot(t)
	projectDir := filepath.Join(t.TempDir(), "project")
	t.Setenv("OV_TEST_OPENCODE_BIN", "/tmp/opencode-test")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"text","part":{"type":"text","text":"OK","time":{"end":1}}}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Exec, "chat", map[string]any{
		"message_template": "ingest {{resource_url}} for {{case_id}}",
		"project_dir":      projectDir,
		"plugin_root":      pluginRoot,
	}).Process(map[string]any{"resource_url": "https://example.test/r.md", "case_id": "opencode-case"})
	if err != nil {
		t.Fatalf("opencode exec should pass: %v", err)
	}
	if got := seen[len(seen)-1]; got != "ingest https://example.test/r.md for opencode-case" {
		t.Fatalf("rendered prompt = %q", got)
	}
}

func TestOpenCodeExecCanRetryEmptyReplyWhenExplicitlyConfigured(t *testing.T) {
	setTargetConf(t)
	pluginRoot := makePluginRoot(t)
	projectDir := filepath.Join(t.TempDir(), "project")
	t.Setenv("OV_TEST_OPENCODE_BIN", "/tmp/opencode-test")

	calls := 0
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		calls++
		if calls == 1 {
			return root.CLIResult{ExitCode: 0, Stdout: `{"type":"step_finish","sessionID":"ses_empty","part":{"type":"step-finish","reason":"stop"}}` + "\n"}
		}
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"text","sessionID":"ses_ok","part":{"type":"text","text":"OVTEST_PREFLIGHT_OK","time":{"end":1}}}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Exec, "preflight", map[string]any{
		"message": "reply", "project_dir": projectDir, "plugin_root": pluginRoot,
		"empty_reply_retries": 1,
	}).Process(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out["reply"] != "OVTEST_PREFLIGHT_OK" {
		t.Fatalf("calls=%d out=%v", calls, out)
	}
	jsonl := out["jsonl"].(string)
	if !strings.Contains(jsonl, "ses_empty") || !strings.Contains(jsonl, "ses_ok") {
		t.Fatalf("combined JSONL did not preserve both attempts: %q", jsonl)
	}
}

func TestOpenCodeExecDoesNotRetryEmptyReplyByDefault(t *testing.T) {
	setTargetConf(t)
	pluginRoot := makePluginRoot(t)
	calls := 0
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		calls++
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Exec, "product_case", map[string]any{
		"message": "reply", "project_dir": t.TempDir(), "plugin_root": pluginRoot,
	}).Process(map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "empty final reply") || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestOpenCodeExecCanAllowEmptyReplyWithoutRetrying(t *testing.T) {
	setTargetConf(t)
	pluginRoot := makePluginRoot(t)
	calls := 0
	const jsonl = `{"type":"tool_use","part":{"type":"tool","tool":"openviking_add_resource","state":{"status":"completed","output":"ok"}}}`
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		calls++
		return root.CLIResult{ExitCode: 0, Stdout: jsonl + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Exec, "state_changing_tool", map[string]any{
		"message": "upload", "project_dir": t.TempDir(), "plugin_root": pluginRoot,
		"allow_empty_reply": true,
	}).Process(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || out["reply"] != "" || !strings.Contains(out["jsonl"].(string), "openviking_add_resource") {
		t.Fatalf("calls=%d out=%v", calls, out)
	}
}

func TestOpenCodeExecCopiesConfigTemplate(t *testing.T) {
	setTargetConf(t)
	pluginRoot := makePluginRoot(t)
	projectDir := filepath.Join(t.TempDir(), "project")
	template := filepath.Join(t.TempDir(), "opencode-template.json")
	if err := os.WriteFile(template, []byte(`{
  // Keep model connectivity, but never inherit ambient behavior.
  "provider": {"test": {}},
  "enabled_providers": ["test"],
  "permission": {"bash": "allow"},
  "mcp": {"unrelated": {"command": "steal"}},
  "plugin": ["ambient-plugin"],
}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_OPENCODE_BIN", "/tmp/opencode-test")

	var env map[string]string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, envExtra map[string]string, _, _ int) root.CLIResult {
		env = envExtra
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"text","part":{"type":"text","text":"OK","time":{"end":1}}}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Exec, "chat", map[string]any{
		"message":                  "hello",
		"project_dir":              projectDir,
		"plugin_root":              pluginRoot,
		"opencode_config_template": template,
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("opencode exec should pass: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(env["OPENCODE_CONFIG_DIR"], "opencode.json"))
	if err != nil {
		t.Fatalf("read copied config template: %v", err)
	}
	if !strings.Contains(string(raw), `"provider"`) || !strings.Contains(string(raw), `"enabled_providers"`) {
		t.Fatalf("copied config template = %q", string(raw))
	}
	for _, forbidden := range []string{"permission", "mcp", "ambient-plugin", "steal"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("sanitized config retained %q: %s", forbidden, raw)
		}
	}
}

func TestOpenCodeExecDefaultDirsAreRunIsolated(t *testing.T) {
	setTargetConf(t)
	pluginRoot := makePluginRoot(t)
	t.Setenv("OV_TEST_OPENCODE_BIN", "/tmp/opencode-test")
	t.Setenv("OV_TEST_OPENCODE_PLUGIN_ROOT", pluginRoot)

	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 0, Stdout: `{"type":"text","part":{"type":"text","text":"OK","time":{"end":1}}}` + "\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	out1, err := newOp(Exec, "chat", map[string]any{"message": "one"}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("first opencode exec should pass: %v", err)
	}
	out2, err := newOp(Exec, "chat", map[string]any{"message": "two"}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("second opencode exec should pass: %v", err)
	}
	if out1["project_dir"] == out2["project_dir"] || out1["state_dir"] == out2["state_dir"] {
		t.Fatalf("default dirs must be run-isolated, got project/state %q/%q then %q/%q",
			out1["project_dir"], out1["state_dir"], out2["project_dir"], out2["state_dir"])
	}
}

func TestOpenCodeEvidenceRequiresCompletedOpenVikingToolEvents(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"tool_use","part":{"type":"tool","tool":"openviking_health","state":{"status":"completed","output":"ok"}}}`,
		`{"type":"tool_use","part":{"type":"tool","tool":"openviking_remember","state":{"status":"completed","output":"remembered mango sticky rice"}}}`,
		`{"type":"tool_use","part":{"type":"tool","tool":"openviking_search","state":{"status":"completed","output":"found new zealand"}}}`,
	}, "\n")

	out, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health", "remember", "search"},
		"expect":       []string{"mango sticky rice", "new zealand"},
	}).Process(map[string]any{"jsonl": jsonl, "reply": "done"})
	if err != nil {
		t.Fatalf("expected tool evidence should pass: %v", err)
	}
	if out["ok"] != true || !strings.Contains(out["text"].(string), "mango sticky rice") {
		t.Fatalf("out = %v", out)
	}
}

func TestOpenCodeEvidenceRejectsPromptOnlyToolNames(t *testing.T) {
	jsonl := `{"type":"text","part":{"type":"text","text":"please call openviking_health"}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("prompt-only tool names must not satisfy evidence, got %v", err)
	}
}

func TestOpenCodeEvidenceRejectsErroredToolCall(t *testing.T) {
	jsonl := `{"type":"tool_use","part":{"type":"tool","tool":"openviking_health","state":{"status":"error","error":"boom"}}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("errored tool call must not satisfy evidence, got %v", err)
	}
}

func TestOpenCodeEvidenceRejectsMissingTool(t *testing.T) {
	jsonl := `{"type":"tool_use","part":{"type":"tool","tool":"openviking_health","state":{"status":"completed","output":"ok"}}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health", "remember"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "remember") {
		t.Fatalf("missing tool must be a gate fail with tool detail, got %v", err)
	}
}

func TestOpenCodeEvidenceRejectsBusinessErrorResult(t *testing.T) {
	jsonl := `{"type":"tool_use","part":{"type":"tool","tool":"openviking_health","state":{"status":"completed","output":"Error: unauthorized"}}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"expect_tools": []string{"health"}}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("business-error result must not satisfy evidence, got %v", err)
	}
}

func TestOpenCodeEvidenceForbidsAttemptedTool(t *testing.T) {
	jsonl := `{"type":"tool_use","part":{"type":"tool","tool":"openviking_search","state":{"status":"pending"}}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"forbid_tools": []string{"search"}}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "search") {
		t.Fatalf("attempted forbidden tool must fail, got %v", err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
