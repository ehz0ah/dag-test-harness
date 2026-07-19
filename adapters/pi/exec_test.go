package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

func newOp(factory dag.Factory, name string, config map[string]any) interface {
	Process(map[string]any) (map[string]any, error)
} {
	return factory.New(name, config)
}

func piTargetConf(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ovcli.conf"), []byte(`{"url":"http://ov.local","api_key":"conf-key","account":"acct","user":"runner"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)
}

func piExtension(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path, body := range map[string]string{
		"index.ts":       "export default function () {}\n",
		"config.json":    "{}\n",
		"lib/helper.mjs": "export const ok = true;\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPiExecUsesRPCIsolatedExtensionAndScopedCredentials(t *testing.T) {
	piTargetConf(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	projectDir := filepath.Join(t.TempDir(), "project")
	secretDir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("OV_TEST_SECRET_STATE_DIR", secretDir)
	t.Setenv("OV_TEST_PI_BIN", "/tmp/pi-test")

	var seenArgv []string
	var seenEnv map[string]string
	var seenMessage string
	var seenCompact bool
	original := runRPC
	runRPC = func(_ context.Context, _ string, argv []string, env map[string]string, message string, compact bool, _ int) root.CLIResult {
		seenArgv = append([]string(nil), argv...)
		seenEnv, seenMessage, seenCompact = env, message, compact
		return root.CLIResult{ExitCode: 0, Stdout: strings.Join([]string{
			`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"pi:session/1","sessionFile":"/tmp/session.jsonl"}}`,
			`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"PI_ACK"}]}}`,
		}, "\n") + "\n"}
	}
	defer func() { runRPC = original }()

	out, err := newOp(Exec, "chat", map[string]any{
		"message": "remember this", "project_dir": projectDir, "state_dir": stateDir,
		"extension_root": piExtension(t), "model": "model-x",
		"llm_base_url": "https://llm.example/v1", "llm_api_key": "llm-secret",
		"auto_capture": true, "takeover": false, "disable_tools": true,
		"recall_peer_scope": "actor", "score_threshold": "0.35",
	}).Process(map[string]any{"user_key": "wired-ov-key"})
	if err != nil {
		t.Fatal(err)
	}
	if seenMessage != "remember this" || seenCompact {
		t.Fatalf("RPC input = %q compact=%v", seenMessage, seenCompact)
	}
	joined := strings.Join(seenArgv, " ")
	for _, expected := range []string{"--mode rpc", "--approve", "--no-extensions", "--extension", "--session-dir", "--model ovtest/model-x", "--no-tools"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("argv omitted %q: %v", expected, seenArgv)
		}
	}
	if seenEnv["HOME"] == os.Getenv("HOME") || seenEnv["PI_CODING_AGENT_DIR"] == "" {
		t.Fatalf("Pi environment is not isolated: %v", seenEnv)
	}
	if seenEnv["OPENVIKING_API_KEY"] != "wired-ov-key" || seenEnv["OV_TEST_PI_LLM_API_KEY"] != "llm-secret" {
		t.Fatalf("typed credentials were not scoped correctly: %v", seenEnv)
	}
	if got := seenEnv["OPENVIKING_RECALL_PEER_SCOPE"]; got != "actor" {
		t.Fatalf("recall peer scope = %q, want actor", got)
	}
	if !strings.HasPrefix(seenEnv["OPENVIKING_QUEUE_SCOPE_KEY_FILE"], secretDir+string(filepath.Separator)) {
		t.Fatalf("queue scope key is not under secret state: %q", seenEnv["OPENVIKING_QUEUE_SCOPE_KEY_FILE"])
	}
	models, err := os.ReadFile(filepath.Join(seenEnv["PI_CODING_AGENT_DIR"], "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(models), "llm-secret") || !strings.Contains(string(models), "$OV_TEST_PI_LLM_API_KEY") {
		t.Fatalf("models config persisted a credential or omitted env indirection: %s", models)
	}
	extensionConfig, err := os.ReadFile(filepath.Join(stateDir, "extension", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(extensionConfig), `"commitKeepRecentCount": 0`) {
		t.Fatalf("test sessions must archive all captured turns: %s", extensionConfig)
	}
	if !strings.Contains(string(extensionConfig), `"scoreThreshold": "0.35"`) {
		t.Fatalf("configured recall threshold was not preserved: %s", extensionConfig)
	}
	settings, err := os.ReadFile(filepath.Join(seenEnv["PI_CODING_AGENT_DIR"], "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settings), `"keepRecentTokens": 20000`) {
		t.Fatalf("default Pi compaction settings changed: %s", settings)
	}
	if out["reply"] != "PI_ACK" || out["ov_session_id"] != "pi-pi-session-1" || out["session_file"] != "/tmp/session.jsonl" {
		t.Fatalf("output = %v", out)
	}
	if _, err := os.Stat(filepath.Join(out["extension_dir"].(string), "lib", "helper.mjs")); err != nil {
		t.Fatalf("isolated extension copy: %v", err)
	}
}

func TestPiStructuredEvidenceFailsClosed(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"tool_execution_end","toolName":"viking_search","isError":false,"result":{"content":[{"type":"text","text":"found"}]}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"mentioned viking_remember only"}]}}`,
		`{"type":"tool_execution_end","toolName":"viking_forget","isError":true,"result":{"content":"failed"}}`,
		`{"type":"tool_execution_end","toolName":"viking_read","isError":false,"result":{"success":false}}`,
	}, "\n")
	tools := completedPiTools(jsonl)
	if !tools.Contains("viking_search") || tools.Contains("viking_remember") || tools.Contains("viking_forget") || tools.Contains("viking_read") {
		t.Fatalf("completed tools = %v", tools)
	}
	_, err := newOp(Evidence, "evidence", map[string]any{"expect_tools": []string{"viking_search", "viking_remember"}}).Process(map[string]any{"jsonl": jsonl})
	if err == nil {
		t.Fatal("prompt-only tool name satisfied Pi evidence")
	}
}

func TestPiToolFreeEvidenceRejectsAnyTool(t *testing.T) {
	jsonl := `{"type":"tool_execution_start","toolName":"bash","args":{"command":"find memory"}}`
	_, err := newOp(Evidence, "evidence", map[string]any{"forbid_any_tool": true}).Process(map[string]any{"jsonl": jsonl})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(strings.ToLower(gf.Detail), "bash") {
		t.Fatalf("attempted tool must fail a tool-free gate, got %v", err)
	}
}

func TestPiCompactionRequiresResponseAndLifecycleEvent(t *testing.T) {
	complete := strings.Join([]string{
		`{"type":"compaction_end","reason":"manual","result":{"summary":"ok"},"aborted":false}`,
		`{"type":"response","command":"compact","success":true,"data":{"summary":"ok"}}`,
	}, "\n")
	if !parsePiRun(complete).Compacted {
		t.Fatal("complete compaction was not recognized")
	}
	if parsePiRun(`{"type":"response","command":"compact","success":true}`).Compacted {
		t.Fatal("response without lifecycle evidence passed")
	}
	if parsePiRun(`{"type":"compaction_end","result":{"summary":"ok"},"aborted":true}`).Compacted {
		t.Fatal("aborted compaction passed")
	}
}
