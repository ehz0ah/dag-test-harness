package codex

import (
	"bytes"
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
	if err := os.WriteFile(path, []byte(`{"url":"http://ov.local","api_key":"conf-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CODEX_AUTH_FILE", authPath)
	pluginRoot := filepath.Join(dir, "OpenViking", "examples", "codex-memory-plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte(`{"name":"openviking-memory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CODEX_PLUGIN_ROOT", pluginRoot)
	originalInstall := installCodexPlugin
	originalTrust := trustCodexHooks
	installCodexPlugin = func(_ *root.OpContext, _, _ string) error { return nil }
	trustCodexHooks = func(context.Context, string, string, string) error { return nil }
	t.Cleanup(func() {
		installCodexPlugin = originalInstall
		trustCodexHooks = originalTrust
	})
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

func TestPrepareCodexHomeKeepsAuthInSecretState(t *testing.T) {
	setTargetConf(t)
	stateDir := filepath.Join(t.TempDir(), "retained-runtime")
	secretDir := filepath.Join(t.TempDir(), "always-deleted")
	t.Setenv("OV_TEST_SECRET_STATE_DIR", secretDir)

	home, _, err := prepareCodexHome("chat", map[string]any{}, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(home, secretDir+string(filepath.Separator)) {
		t.Fatalf("CODEX_HOME %q is not below secret root %q", home, secretDir)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatalf("secret auth copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "codex-home", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("auth must not be retained in runtime, stat error = %v", err)
	}
}

func TestCodexEnvironmentKeepsOpenVikingCredentialsInSecretState(t *testing.T) {
	setTargetConf(t)
	stateDir := filepath.Join(t.TempDir(), "retained-runtime")
	secretDir := filepath.Join(t.TempDir(), "always-deleted")
	t.Setenv("OV_TEST_SECRET_STATE_DIR", secretDir)
	queueKey := filepath.Join(secretDir, "environment-queue.key")
	t.Setenv("OPENVIKING_QUEUE_SCOPE_KEY_FILE", queueKey)

	env, _, _, cliConfigPath, err := codexOpenVikingEnv("chat", map[string]any{
		"state_dir": stateDir, "openviking_url": "http://ov.local",
	}, "wired-key", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cliConfigPath, secretDir+string(filepath.Separator)) {
		t.Fatalf("CLI config %q is not below secret root %q", cliConfigPath, secretDir)
	}
	if got := env["HOME"]; !strings.HasPrefix(got, secretDir+string(filepath.Separator)) {
		t.Fatalf("isolated HOME %q is not below secret root %q", got, secretDir)
	}
	if got := env["OPENVIKING_QUEUE_SCOPE_KEY_FILE"]; got != queueKey {
		t.Fatalf("queue scope key = %q, want %q", got, queueKey)
	}
	defaultConfig := filepath.Join(env["HOME"], ".openviking", "ovcli.conf")
	if raw, err := os.ReadFile(defaultConfig); err != nil || !strings.Contains(string(raw), "wired-key") {
		t.Fatalf("default OpenViking config = %q, err = %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "ovcli.conf")); !os.IsNotExist(err) {
		t.Fatalf("OpenViking credentials must not be retained in runtime, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "home", ".openviking", "ovcli.conf")); !os.IsNotExist(err) {
		t.Fatalf("default OpenViking credentials must not be retained in runtime, stat error = %v", err)
	}
}

func TestCodexExecArgvEnvAndReply(t *testing.T) {
	setTargetConf(t)
	cwd := filepath.Join(t.TempDir(), "codex-cwd")
	stateDir := filepath.Join(t.TempDir(), "codex-state")
	t.Setenv("OV_TEST_CODEX_BIN", "/tmp/codex-test")

	var seen []string
	var env map[string]string
	var timeout int
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, envExtra map[string]string, _, timeoutSec int) root.CLIResult {
		seen = append([]string(nil), argv...)
		env = envExtra
		timeout = timeoutSec
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--output-last-message" {
				if err := os.WriteFile(argv[i+1], []byte("ACK from codex\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return root.CLIResult{
			ExitCode: 0,
			Stdout: strings.Join([]string{
				`{"type":"thread.started","thread_id":"codex:session"}`,
				`{"type":"agent_message","message":"ACK from jsonl"}`,
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
		"auto_capture":          false,
		"auto_recall":           true,
		"bypass_approvals":      true,
		"model":                 "test-model",
		"capture_timeout_ms":    "9000",
		"recall_timeout_ms":     "8000",
		"score_threshold":       "0.12",
		"active_window_ms":      "7000",
		"openviking_peer_id":    "peer-1",
		"openviking_extra_noop": "ignored",
	}).Process(map[string]any{"user_key": "wired-key"})
	if err != nil {
		t.Fatalf("codex exec should pass: %v", err)
	}
	if len(seen) < 4 || seen[0] != "/tmp/codex-test" || seen[1] != "exec" || seen[2] != "-c" ||
		!strings.HasPrefix(seen[3], "shell_environment_policy.set={") ||
		!strings.Contains(seen[3], "OPENVIKING_CLI_CONFIG_FILE") ||
		!strings.Contains(seen[3], `OPENVIKING_CREDENTIAL_SOURCE = "cli"`) {
		t.Fatalf("argv did not include Codex shell env override: %v", seen)
	}
	wantArgv := []string{
		"/tmp/codex-test", "exec",
		"-c", seen[3],
		"--json",
		"--skip-git-repo-check",
		"-C", cwd,
		"--output-last-message", filepath.Join(cwd, "chat.last-message.txt"),
		"--dangerously-bypass-approvals-and-sandbox",
		"--model", "test-model",
		"use OpenViking",
	}
	if !equalStrs(seen, wantArgv) {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
	if timeout != 41 {
		t.Fatalf("timeout = %d, want 41", timeout)
	}
	if out["ov_session_id"] != "cx-codex_session" {
		t.Fatalf("ov_session_id = %v", out["ov_session_id"])
	}
	if env["CODEX_HOME"] != filepath.Join(stateDir, "codex-home") ||
		env["OPENVIKING_CREDENTIAL_SOURCE"] != "cli" ||
		env["OPENVIKING_CLI_CONFIG_FILE"] == "" ||
		env["OPENVIKING_URL"] != "http://cfg.ov" ||
		env["OPENVIKING_API_KEY"] != "cfg-key" ||
		env["OPENVIKING_AUTH_MODE"] != "api_key" ||
		env["OPENVIKING_CODEX_STATE_DIR"] != stateDir ||
		env["OPENVIKING_DEBUG"] != "1" ||
		env["OPENVIKING_AUTO_CAPTURE"] != "0" ||
		env["OPENVIKING_AUTO_RECALL"] != "1" ||
		env["OPENVIKING_CAPTURE_TIMEOUT_MS"] != "9000" ||
		env["OPENVIKING_RECALL_TIMEOUT_MS"] != "8000" ||
		env["OPENVIKING_SCORE_THRESHOLD"] != "0.12" ||
		env["OPENVIKING_CODEX_ACTIVE_WINDOW_MS"] != "7000" ||
		env["OPENVIKING_PEER_ID"] != "peer-1" {
		t.Fatalf("env = %v", env)
	}
	rawConf, err := os.ReadFile(env["OPENVIKING_CLI_CONFIG_FILE"])
	if err != nil {
		t.Fatalf("read generated ovcli config: %v", err)
	}
	var cliConf map[string]any
	if err := json.Unmarshal(rawConf, &cliConf); err != nil {
		t.Fatalf("decode generated ovcli config: %v", err)
	}
	if cliConf["url"] != "http://cfg.ov" || cliConf["api_key"] != "cfg-key" || cliConf["actor_peer_id"] != "peer-1" {
		t.Fatalf("generated ovcli config = %v", cliConf)
	}
	defaultRaw, err := os.ReadFile(filepath.Join(env["HOME"], ".openviking", "ovcli.conf"))
	if err != nil {
		t.Fatalf("read default generated ovcli config: %v", err)
	}
	if string(defaultRaw) != string(rawConf) {
		t.Fatalf("default ovcli config differs from explicit config")
	}
	if out["reply"] != "ACK from codex" || out["jsonl"] == "" ||
		out["jsonl_path"] != filepath.Join(cwd, "chat.jsonl") ||
		out["state_dir"] != stateDir ||
		out["cli_config_path"] != env["OPENVIKING_CLI_CONFIG_FILE"] {
		t.Fatalf("out = %v", out)
	}
	isolatedConfig, err := os.ReadFile(filepath.Join(env["CODEX_HOME"], "config.toml"))
	if err != nil {
		t.Fatalf("read isolated Codex config: %v", err)
	}
	if !strings.Contains(string(isolatedConfig), `[plugins."openviking-memory@openviking"]`) ||
		!strings.Contains(string(isolatedConfig), `hooks = true`) {
		t.Fatalf("isolated Codex config = %q", string(isolatedConfig))
	}
	if _, err := os.Stat(filepath.Join(env["CODEX_HOME"], "auth.json")); err != nil {
		t.Fatalf("isolated Codex auth copy: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(cwd, "chat.jsonl")); err != nil || !strings.Contains(string(raw), "ACK from jsonl") {
		t.Fatalf("jsonl evidence was not persisted: raw=%q err=%v", string(raw), err)
	}
}

func TestInstallCodexPluginUsesIsolatedHome(t *testing.T) {
	var argv []string
	var env map[string]string
	original := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, gotArgv []string, gotEnv map[string]string, _, _ int) root.CLIResult {
		argv = append([]string(nil), gotArgv...)
		env = gotEnv
		return root.CLIResult{ExitCode: 0}
	}
	defer func() { root.RunCLIContext = original }()

	// Exercise the production installer through a minimal OpContext created by
	// a dedicated factory so subprocess cancellation remains DAG-scoped.
	factory := root.NewFactory(dag.Meta{}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(map[string]any) (map[string]any, error) {
			return nil, installCodexPluginCommand(ctx, "/tmp/codex", "/tmp/codex-home")
		}
	})
	if _, err := factory.New("install", nil).Process(nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"/tmp/codex", "plugin", "add", "openviking-memory@openviking"}
	if !equalStrs(argv, want) || env["CODEX_HOME"] != "/tmp/codex-home" {
		t.Fatalf("install argv/env = %v / %v", argv, env)
	}
}

func TestTrustCodexPluginHooksUsesCodexHashesAndOnlyPluginHooks(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "write-request.json")
	homeCapture := filepath.Join(dir, "codex-home.txt")
	t.Setenv("OVTEST_CODEX_RPC_CAPTURE", capture)
	t.Setenv("OVTEST_CODEX_HOME_CAPTURE", homeCapture)
	binary := writeFakeCodexAppServer(t, dir, `[
  {"key":"ov:ss","eventName":"sessionStart","source":"plugin","pluginId":"openviking-memory@openviking","enabled":true,"currentHash":"sha256:ss"},
  {"key":"ov:prompt","eventName":"userPromptSubmit","source":"plugin","pluginId":"openviking-memory@openviking","enabled":true,"currentHash":"sha256:prompt"},
  {"key":"ov:stop","eventName":"stop","source":"plugin","pluginId":"openviking-memory@openviking","enabled":true,"currentHash":"sha256:stop"},
  {"key":"ov:compact","eventName":"preCompact","source":"plugin","pluginId":"openviking-memory@openviking","enabled":true,"currentHash":"sha256:compact"},
  {"key":"user:other","eventName":"stop","source":"user","pluginId":null,"enabled":true,"currentHash":"sha256:other"}
]`)

	codexHome := filepath.Join(dir, "isolated-home")
	if err := trustCodexPluginHooks(context.Background(), binary, codexHome, dir); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(homeCapture); err != nil || strings.TrimSpace(string(raw)) != codexHome {
		t.Fatalf("CODEX_HOME capture = %q, err = %v", raw, err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Method string `json:"method"`
		Params struct {
			Edits []struct {
				KeyPath string                    `json:"keyPath"`
				Value   map[string]map[string]any `json:"value"`
			} `json:"edits"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "config/batchWrite" || len(request.Params.Edits) != 1 || request.Params.Edits[0].KeyPath != "hooks.state" {
		t.Fatalf("write request = %s", raw)
	}
	states := request.Params.Edits[0].Value
	if len(states) != 4 || states["user:other"] != nil || states["ov:ss"]["trusted_hash"] != "sha256:ss" {
		t.Fatalf("trusted hook states = %#v", states)
	}
}

func TestTrustCodexPluginHooksRejectsMissingRequiredEvent(t *testing.T) {
	dir := t.TempDir()
	binary := writeFakeCodexAppServer(t, dir, `[
  {"key":"ov:ss","eventName":"sessionStart","source":"plugin","pluginId":"openviking-memory@openviking","enabled":true,"currentHash":"sha256:ss"}
]`)
	err := trustCodexPluginHooks(context.Background(), binary, filepath.Join(dir, "home"), dir)
	if err == nil || !strings.Contains(err.Error(), "missing required hook event") {
		t.Fatalf("error = %v", err)
	}
}

func writeFakeCodexAppServer(t *testing.T, dir, hooksJSON string) string {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(hooksJSON)); err != nil {
		t.Fatal(err)
	}
	hooksJSON = compact.String()
	path := filepath.Join(dir, "codex")
	script := `#!/bin/sh
IFS= read -r initialize
printf '%s\n' '{"id":0,"result":{"codexHome":"test"}}'
IFS= read -r initialized
IFS= read -r list
printf '%s\n' '{"id":1,"result":{"data":[{"hooks":` + hooksJSON + `,"warnings":[],"errors":[]}]}}'
IFS= read -r write_request || exit 0
printf '%s\n' "$write_request" > "$OVTEST_CODEX_RPC_CAPTURE"
printf '%s\n' "$CODEX_HOME" > "$OVTEST_CODEX_HOME_CAPTURE"
printf '%s\n' '{"id":2,"result":{"filePath":"config.toml","status":"ok"}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexExecRendersMessageTemplate(t *testing.T) {
	setTargetConf(t)
	cwd := filepath.Join(t.TempDir(), "codex-cwd")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--output-last-message" {
				_ = os.WriteFile(argv[i+1], []byte("OK\n"), 0o600)
			}
		}
		return root.CLIResult{ExitCode: 0, Stdout: "{}\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Exec, "chat", map[string]any{
		"message_template": "ingest {{resource_url}} for {{case_id}}",
		"cwd":              cwd,
	}).Process(map[string]any{"resource_url": "https://example.test/r.md", "case_id": "codex-case"})
	if err != nil {
		t.Fatalf("codex exec should pass: %v", err)
	}
	if got := seen[len(seen)-1]; got != "ingest https://example.test/r.md for codex-case" {
		t.Fatalf("rendered prompt = %q", got)
	}
}

func TestCodexExecDefaultDirsAreRunIsolated(t *testing.T) {
	setTargetConf(t)
	t.Setenv("OV_TEST_CODEX_BIN", "/tmp/codex-test")

	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--output-last-message" {
				_ = os.MkdirAll(filepath.Dir(argv[i+1]), 0o700)
				_ = os.WriteFile(argv[i+1], []byte("OK\n"), 0o600)
			}
		}
		return root.CLIResult{ExitCode: 0, Stdout: "{}\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	out1, err := newOp(Exec, "chat", map[string]any{"message": "one"}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("first codex exec should pass: %v", err)
	}
	out2, err := newOp(Exec, "chat", map[string]any{"message": "two"}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("second codex exec should pass: %v", err)
	}
	if out1["cwd"] == out2["cwd"] || out1["state_dir"] == out2["state_dir"] {
		t.Fatalf("default dirs must be run-isolated, got cwd/state %q/%q then %q/%q",
			out1["cwd"], out1["state_dir"], out2["cwd"], out2["state_dir"])
	}
}

func TestCodexExecClassifiesStructuredUsageLimitAsEnvironmentFailure(t *testing.T) {
	setTargetConf(t)
	cwd := filepath.Join(t.TempDir(), "codex-cwd")

	original := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 1, Stdout: strings.Join([]string{
			`{"type":"thread.started","thread_id":"thread-1"}`,
			`{"type":"error","message":"You've hit your usage limit. Visit settings to purchase more credits."}`,
			`{"type":"turn.failed","error":{"message":"You've hit your usage limit. Visit settings to purchase more credits."}}`,
		}, "\n") + "\n", Stderr: "unrelated warning"}
	}
	defer func() { root.RunCLIContext = original }()

	_, err := newOp(Exec, "chat", map[string]any{"message": "hello", "cwd": cwd}).Process(map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "Codex environment unavailable") || !strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("error = %v", err)
	}
	var gateFail *root.GateFail
	if errors.As(err, &gateFail) {
		t.Fatalf("usage exhaustion must be an environment failure, got gate failure: %v", err)
	}
	if raw, readErr := os.ReadFile(filepath.Join(cwd, "chat.jsonl")); readErr != nil || !strings.Contains(string(raw), "usage limit") {
		t.Fatalf("structured failure evidence was not persisted: raw=%q err=%v", raw, readErr)
	}
}

func TestCodexExecDoesNotReclassifyOrdinaryTurnFailure(t *testing.T) {
	setTargetConf(t)
	original := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 1, Stdout: `{"type":"turn.failed","error":{"message":"tool execution failed"}}` + "\n"}
	}
	defer func() { root.RunCLIContext = original }()

	_, err := newOp(Exec, "chat", map[string]any{"message": "hello", "cwd": t.TempDir()}).Process(map[string]any{})
	var gateFail *root.GateFail
	if !errors.As(err, &gateFail) {
		t.Fatalf("ordinary Codex failure must remain candidate-sensitive, got %v", err)
	}
}

func TestCodexExecIsolatedMCPConfig(t *testing.T) {
	setTargetConf(t)
	cwd := filepath.Join(t.TempDir(), "codex-cwd")
	pluginRoot := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, "servers"), 0o700); err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(pluginRoot, "servers", "mcp-proxy.mjs")
	if err := os.WriteFile(proxyPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CODEX_BIN", "/tmp/codex-test")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--output-last-message" {
				_ = os.WriteFile(argv[i+1], []byte("OK\n"), 0o600)
			}
		}
		return root.CLIResult{ExitCode: 0, Stdout: "{}\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Exec, "chat", map[string]any{
		"message":             "use OpenViking MCP",
		"cwd":                 cwd,
		"state_dir":           filepath.Join(t.TempDir(), "state"),
		"openviking_endpoint": "http://cfg.ov",
		"openviking_api_key":  "cfg-key",
		"codex_plugin_root":   pluginRoot,
		"isolated_mcp":        true,
	}).Process(map[string]any{"user_key": "wired-key"})
	if err != nil {
		t.Fatalf("codex exec should pass: %v", err)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{"--strict-config"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, seen)
		}
	}
	if strings.Contains(joined, "mcp_servers.") || strings.Contains(joined, "cfg-key") {
		t.Fatalf("MCP configuration or credential leaked to argv: %v", seen)
	}
	configPath := root.AsString(out["codex_config_path"])
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPluginRoot, err := filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[mcp_servers."openviking-memory"]`, filepath.Join(canonicalPluginRoot, "servers", "mcp-proxy.mjs"),
		"cwd = " + tomlQuote(canonicalPluginRoot), "OPENVIKING_CLI_CONFIG_FILE", `OPENVIKING_CREDENTIAL_SOURCE = "cli"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("MCP config missing %q: %s", want, raw)
		}
	}
}

func TestCodexEvidenceRequiresActualMCPToolEvents(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"user_message","message":"please use MCP health remember search read"}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"openviking-memory","tool":"health","status":"completed","error":null,"result":{"content":[{"type":"text","text":"ok"}]}}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"openviking-memory","tool_name":"remember","status":"completed","error":null,"result":{"content":[{"type":"text","text":"ok"}]}}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"openviking-memory","name":"search","status":"completed","error":null,"result":{"content":[{"type":"text","text":"ok"}]}}}`,
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

func TestCodexEvidenceRejectsPromptOnlyToolNames(t *testing.T) {
	jsonl := `{"type":"user_message","server":"openviking-memory","name":"health","message":"not a tool event"}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("prompt-only tool names must not satisfy evidence, got %v", err)
	}
}

func TestCodexEvidenceRejectsIncompleteToolCall(t *testing.T) {
	jsonl := `{"type":"item.started","item":{"type":"mcp_tool_call","server":"openviking-memory","tool":"health","status":"in_progress"}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("incomplete tool call must not satisfy evidence, got %v", err)
	}
}

func TestCodexEvidenceRejectsMissingTool(t *testing.T) {
	jsonl := `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"openviking-memory","tool":"health","status":"completed","error":null,"result":{"content":[{"type":"text","text":"ok"}]}}}`

	_, err := newOp(Evidence, "evidence", map[string]any{
		"expect_tools": []string{"health", "remember"},
	}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "remember") {
		t.Fatalf("missing tool must be a gate fail with tool detail, got %v", err)
	}
}

func TestCodexEvidenceRejectsBusinessErrorResult(t *testing.T) {
	jsonl := `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"openviking-memory","tool":"health","status":"completed","error":null,"result":{"content":[{"type":"text","text":"Error: unauthorized"}]}}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"expect_tools": []string{"health"}}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "health") {
		t.Fatalf("business-error result must not satisfy evidence, got %v", err)
	}
}

func TestCodexEvidenceForbidsAttemptedTool(t *testing.T) {
	jsonl := `{"type":"item.started","item":{"type":"mcp_tool_call","server":"openviking-memory","tool":"search","status":"in_progress"}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"forbid_tools": []string{"search"}}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "search") {
		t.Fatalf("attempted forbidden tool must fail, got %v", err)
	}
}

func TestExtractLastMessageFromJSONLHandlesEscapes(t *testing.T) {
	jsonl := `{"type":"agent_message","text":"ACK with \"quote\" done"}`

	got := extractLastMessageFromJSONL(jsonl)
	if got != `ACK with "quote" done` {
		t.Fatalf("reply = %q, want escaped JSON text decoded", got)
	}
}
