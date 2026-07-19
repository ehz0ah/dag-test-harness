package hermes

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
}

func stubCLI(stdout, stderr string) func() {
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 0, Stdout: stdout, Stderr: stderr}
	}
	return func() { root.RunCLIContext = orig }
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
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

func TestHermesChatArgvEnvAndSessionID(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")
	t.Setenv("OV_TEST_HERMES_BIN", "/tmp/hermes-test")

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
			Stdout:   "The story has been acknowledged.\n",
			Stderr:   "trace line\nsession_id: hermes-session-123\n",
		}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Chat, "chat", map[string]any{
		"message":               "a plain story turn",
		"home":                  home,
		"timeout":               41,
		"openviking_endpoint":   "http://cfg.ov",
		"openviking_api_key":    "cfg-key",
		"openviking_account":    "acct",
		"openviking_user":       "user",
		"openviking_agent":      "agent-x",
		"openviking_sync_trace": "1",
	}).Process(map[string]any{"user_key": "wired-key"})
	if err != nil {
		t.Fatalf("hermes chat should pass: %v", err)
	}
	wantArgv := []string{"/tmp/hermes-test", "--cli", "chat", "-q", "a plain story turn", "--quiet"}
	if !equalStrs(seen, wantArgv) {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
	if timeout != 41 {
		t.Fatalf("timeout = %d, want 41", timeout)
	}
	if env["HOME"] != home ||
		env["HERMES_HOME"] != home ||
		env["OPENVIKING_ENDPOINT"] != "http://cfg.ov" ||
		env["OPENVIKING_API_KEY"] != "cfg-key" ||
		env["OPENVIKING_ACCOUNT"] != "acct" ||
		env["OPENVIKING_USER"] != "user" ||
		env["OPENVIKING_AGENT"] != "agent-x" ||
		env["HERMES_OPENVIKING_SYNC_TRACE"] != "1" {
		t.Fatalf("env = %v", env)
	}
	if out["reply"] != "The story has been acknowledged." || out["session_id"] != "hermes-session-123" {
		t.Fatalf("out = %v", out)
	}
}

func TestHermesChatCanPinToolsets(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")
	t.Setenv("OV_TEST_HERMES_BIN", "/tmp/hermes-test")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: "ack\n", Stderr: "session_id: s-1\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Chat, "chat", map[string]any{
		"message": "story", "home": home, "toolsets": "web",
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("hermes chat should pass: %v", err)
	}
	wantArgv := []string{"/tmp/hermes-test", "--cli", "chat", "-q", "story", "--quiet", "--toolsets", "web"}
	if !equalStrs(seen, wantArgv) {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
}

func TestHermesChatOmitsEmptyToolsets(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")
	t.Setenv("OV_TEST_HERMES_BIN", "/tmp/hermes-test")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: "ack\n", Stderr: "session_id: s-1\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Chat, "chat", map[string]any{
		"message": "story", "home": home, "toolsets": []string{},
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("hermes chat should pass: %v", err)
	}
	wantArgv := []string{"/tmp/hermes-test", "--cli", "chat", "-q", "story", "--quiet"}
	if !equalStrs(seen, wantArgv) {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
}

func TestHermesChatRendersMessageTemplateFromInputs(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")
	t.Setenv("OV_TEST_HERMES_BIN", "/tmp/hermes-test")

	var seen []string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, _ map[string]string, _, _ int) root.CLIResult {
		seen = append([]string(nil), argv...)
		return root.CLIResult{ExitCode: 0, Stdout: "ack\n", Stderr: "session_id: s-1\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Chat, "chat", map[string]any{
		"message_template": "ingest {{resource_url}} for {{case_id}}",
		"home":             home,
	}).Process(map[string]any{"resource_url": "http://127.0.0.1/f.txt", "case_id": "resource-case"})
	if err != nil {
		t.Fatalf("hermes chat should pass: %v", err)
	}
	wantArgv := []string{"/tmp/hermes-test", "--cli", "chat", "-q", "ingest http://127.0.0.1/f.txt for resource-case", "--quiet"}
	if !equalStrs(seen, wantArgv) {
		t.Fatalf("argv = %v, want %v", seen, wantArgv)
	}
}

func TestHermesEvidenceCheckRejectsForbiddenTool(t *testing.T) {
	home := filepath.Join(t.TempDir(), "hermes-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := stubHermesStateQuery([]byte(`[{"id":1,"role":"assistant","content":"","tool_call_id":"","tool_calls":"[{\"id\":\"call-1\",\"function\":{\"name\":\"viking_search\"}}]","tool_name":""}]`))
	defer restore()

	_, err := newOp(Evidence, "no_memory_tools", map[string]any{
		"forbid_tools": []string{"viking_search", "viking_read"},
	}).Process(map[string]any{"home": home})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "viking_search") {
		t.Fatalf("forbidden tool must be a gate fail with token detail, got %v", err)
	}
}

func TestHermesEvidenceIgnoresRegisteredToolCatalog(t *testing.T) {
	home := filepath.Join(t.TempDir(), "hermes-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := stubHermesStateQuery([]byte(`[{"id":1,"role":"system","content":"registered tools: viking_search viking_read","tool_call_id":"","tool_calls":"","tool_name":""}]`))
	defer restore()

	_, err := newOp(Evidence, "no_memory_tools", map[string]any{
		"forbid": []string{"viking_search", "viking_read", "viking_browse", "viking_remember", "viking_forget", "viking_add_resource"},
	}).Process(map[string]any{"home": home})
	if err != nil {
		t.Fatalf("registered catalog names should not count as tool-call evidence: %v", err)
	}
}

func TestHermesEvidenceCheckRequiresExpectedTool(t *testing.T) {
	home := filepath.Join(t.TempDir(), "hermes-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := stubHermesStateQuery([]byte(`[
{"id":1,"role":"assistant","content":"","tool_call_id":"","tool_calls":"[{\"id\":\"call-1\",\"function\":{\"name\":\"viking_remember\"}}]","tool_name":""},
{"id":2,"role":"tool","content":"{\"ok\":true,\"uri\":\"viking://user/memories/x\"}","tool_call_id":"call-1","tool_calls":"","tool_name":"viking_remember"}
]`))
	defer restore()

	out, err := newOp(Evidence, "remember_evidence", map[string]any{
		"expect_tools": []string{"viking_remember"},
		"forbid_tools": []string{"viking_forget"},
	}).Process(map[string]any{"home": home})
	if err != nil {
		t.Fatalf("expected tool evidence should pass: %v", err)
	}
	if out["ok"] != true || !strings.Contains(out["text"].(string), "viking://user/memories/x") {
		t.Fatalf("out = %v", out)
	}
}

func TestHermesEvidenceRejectsCompletedBusinessError(t *testing.T) {
	home := filepath.Join(t.TempDir(), "hermes-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := stubHermesStateQuery([]byte(`[
{"id":1,"role":"assistant","content":"","tool_call_id":"","tool_calls":"[{\"id\":\"call-1\",\"function\":{\"name\":\"viking_remember\"}}]","tool_name":""},
{"id":2,"role":"tool","content":"Error: unauthorized","tool_call_id":"call-1","tool_calls":"","tool_name":"viking_remember"}
]`))
	defer restore()
	_, err := newOp(Evidence, "remember_evidence", map[string]any{"expect_tools": []string{"viking_remember"}}).Process(map[string]any{"home": home})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !strings.Contains(gf.Detail, "viking_remember") {
		t.Fatalf("business-error result must not satisfy evidence, got %v", err)
	}
}

func stubHermesStateQuery(result []byte) func() {
	orig := queryHermesState
	queryHermesState = func(string) ([]byte, error) { return result, nil }
	return func() { queryHermesState = orig }
}

func TestHermesPythonPrefersExplicitThenHermesVenv(t *testing.T) {
	t.Setenv("OV_TEST_HERMES_PYTHON", "/explicit/python")
	if got, err := hermesPythonBin(); err != nil || got != "/explicit/python" {
		t.Fatalf("explicit Python = %q, %v", got, err)
	}
	t.Setenv("OV_TEST_HERMES_PYTHON", "")
	binDir := t.TempDir()
	python := filepath.Join(binDir, "python3")
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_HERMES_BIN", filepath.Join(binDir, "hermes"))
	if got, err := hermesPythonBin(); err != nil || got != python {
		t.Fatalf("sibling Python = %q, %v", got, err)
	}
}

func TestHermesStateQueryReportsPythonFailure(t *testing.T) {
	t.Setenv("OV_TEST_HERMES_PYTHON", "/usr/bin/false")
	_, err := queryHermesState(filepath.Join(t.TempDir(), "state.db"))
	if err == nil || !strings.Contains(err.Error(), "Python sqlite3 query failed") {
		t.Fatalf("query failure = %v", err)
	}
}

func TestParseHermesEvidenceRowsRejectsInvalidSchema(t *testing.T) {
	_, _, _, err := parseHermesEvidenceRows([]byte(`{"rows":[]}`))
	if err == nil || !strings.Contains(err.Error(), "decode Hermes state evidence") {
		t.Fatalf("invalid schema error = %v", err)
	}
}

func TestHermesChatDefaultsOpenVikingFromConfiguredUser(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")

	var env map[string]string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, envExtra map[string]string, _, _ int) root.CLIResult {
		env = envExtra
		return root.CLIResult{ExitCode: 0, Stdout: "ack\n", Stderr: "session_id: s-1\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Chat, "chat", map[string]any{
		"message": "story", "home": home,
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("hermes chat should use ovcli.conf defaults: %v", err)
	}
	if env["OPENVIKING_ENDPOINT"] != "http://ov.local" || env["OPENVIKING_API_KEY"] != "conf-key" {
		t.Fatalf("default OpenViking env = %v", env)
	}
	if _, ok := env["OPENVIKING_ACCOUNT"]; ok {
		t.Fatalf("account should not be injected when api key is available: %v", env)
	}
	if _, ok := env["OPENVIKING_USER"]; ok {
		t.Fatalf("user should not be injected when api key is available: %v", env)
	}
}

func TestHermesChatCopiesTemplateConfigIntoIsolatedHome(t *testing.T) {
	setTargetConf(t)
	tempRoot := t.TempDir()
	templateHome := filepath.Join(tempRoot, "template")
	home := filepath.Join(tempRoot, "hermes-home")
	if err := os.MkdirAll(templateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateHome, "config.yaml"), []byte("model:\n  default: test-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateHome, ".env"), []byte("ARK_API_KEY=placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateHome, "session.sqlite"), []byte("state must not copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_HERMES_HOME_TEMPLATE", templateHome)

	var copiedConfig, copiedEnv, copiedState bool
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		_, configErr := os.Stat(filepath.Join(home, "config.yaml"))
		_, envErr := os.Stat(filepath.Join(home, ".env"))
		_, stateErr := os.Stat(filepath.Join(home, "session.sqlite"))
		copiedConfig = configErr == nil
		copiedEnv = envErr == nil
		copiedState = stateErr == nil
		return root.CLIResult{ExitCode: 0, Stdout: "ack\n", Stderr: "session_id: s-1\n"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Chat, "chat", map[string]any{
		"message": "story", "home": home,
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("hermes chat should pass: %v", err)
	}
	if !copiedConfig || !copiedEnv {
		t.Fatalf("template config copied: config=%v env=%v", copiedConfig, copiedEnv)
	}
	if copiedState {
		t.Fatalf("template state file should not be copied into isolated Hermes home")
	}
}

func TestHermesChatRequiresReportedSessionID(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")
	restore := stubCLI("ack\n", "trace without session id\n")
	defer restore()

	_, err := newOp(Chat, "chat", map[string]any{
		"message": "story", "home": home,
	}).Process(map[string]any{})
	var gf *root.GateFail
	if !errors.As(err, &gf) || !contains(gf.Detail, "session_id") {
		t.Fatalf("missing session id must be a gate fail, got %v", err)
	}
}

func TestHermesChatIncludesErrorLogDiagnostic(t *testing.T) {
	setTargetConf(t)
	home := filepath.Join(t.TempDir(), "hermes-home")
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "logs", "errors.log"), []byte("old warning\nprovider rejected unsupported field\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 1, Stderr: "session_id: failed-session"}
	}
	defer func() { root.RunCLIContext = original }()

	_, err := newOp(Chat, "chat", map[string]any{"message": "hello", "home": home}).Process(nil)
	var gate *root.GateFail
	if !errors.As(err, &gate) || !strings.Contains(gate.Detail, "provider rejected unsupported field") {
		t.Fatalf("failure detail = %v", err)
	}
}
