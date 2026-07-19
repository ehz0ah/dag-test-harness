package openclaw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// openclaw_chat uses local `openclaw agent --local`: top-level
// {payloads,meta}, no gateway {runId,status,result} envelope.

func newOp(factory dag.Factory, name string, oc map[string]any) interface {
	Process(map[string]any) (map[string]any, error)
} {
	return factory.New(name, oc)
}

func stubCLI(stdout, stderr string) func() {
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 0, Stdout: stdout, Stderr: stderr}
	}
	return func() { root.RunCLIContext = orig }
}

func stubOv(fn func(args []string, conf string, settle int) root.CLIResult) func() {
	orig := root.RunOvContext
	root.RunOvContext = func(_ context.Context, args []string, conf string, settle int) root.CLIResult {
		return fn(args, conf, settle)
	}
	return func() { root.RunOvContext = orig }
}

func embedded(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "[plugins] warning line\n" + string(b) + "\n[done]\n"
}

func gateway(result map[string]any) string {
	b, _ := json.Marshal(map[string]any{"runId": "r-1", "status": "ok",
		"summary": "completed", "result": result})
	return "[plugins] warning line\n" + string(b) + "\n[done]\n"
}

func gwMeta() map[string]any {
	return map[string]any{"stopReason": "stop",
		"agentMeta":          map[string]any{"sessionId": "12345678-1234-1234-1234-123456789abc"},
		"systemPromptReport": map[string]any{"sessionKey": "agent:main:explicit:s1"}}
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

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func hasStr(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
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

// ── pure helpers ────────────────────────────────────────────────────────────--

func TestOpenclawHelpers(t *testing.T) {
	noisy := "[plugins] warn {not json}\n{\"phase\":\"start\"}\nlog\n" +
		"{\"payloads\":[{\"text\":\"hi\"}],\"meta\":{\"stopReason\":\"stop\"}}\n[done]\n"
	env := parseOpenclawJSON(noisy)
	if m, _ := env["meta"].(map[string]any); m["stopReason"] != "stop" {
		t.Errorf("parse last object: %v", env)
	}
	if parseOpenclawJSON("no json here") != nil && len(parseOpenclawJSON("no json here")) != 0 {
		t.Error("no json -> empty map")
	}
	obj := map[string]any{"a": map[string]any{"b": []any{map[string]any{"sessionKey": "k1"}}}}
	if walkFind(obj, "sessionKey") != "k1" || walkFind(obj, "missing") != nil {
		t.Error("walkFind")
	}
	if openclawReply(map[string]any{"payloads": []any{
		map[string]any{"text": "Hello"}, map[string]any{"text": "world"}, map[string]any{"mediaUrl": "x"}}}) != "Hello world" {
		t.Error("openclawReply concat")
	}
	// local lane: sha256 of the session key; UUIDs pass through lowercased.
	sid := "ovclaw-cap-ab12"
	h := sha256.Sum256([]byte("agent:main:explicit:" + sid))
	if SessionURI(sid, "main") != "viking://session/"+hex.EncodeToString(h[:])+"/messages.jsonl" {
		t.Error("SessionURI sha256")
	}
	u := "ABCDEF12-1234-1234-1234-123456789ABC"
	if ovURIFor(u, "ignored") != "viking://session/abcdef12-1234-1234-1234-123456789abc/messages.jsonl" {
		t.Error("ovURIFor uuid lowercased")
	}
}

func TestParseOpenclawCLIJSONDoesNotLetStderrDiagnosticsReplaceStdout(t *testing.T) {
	r := root.CLIResult{
		Stdout: `{"payloads":[{"text":"remembered"}],"meta":{"stopReason":"stop"}}`,
		Stderr: `[plugin] diagnostic {"phase":"recall","count":1}`,
	}
	result := parseOpenclawCLIJSON(r)
	if got := openclawReply(result); got != "remembered" {
		t.Fatalf("reply = %q, result = %v", got, result)
	}
}

func TestOpenclawBinOverride(t *testing.T) {
	t.Setenv("OV_TEST_OPENCLAW_BIN", "/prepared/openclaw")
	if got := openclawBin(); got != "/prepared/openclaw" {
		t.Fatalf("openclawBin() = %q", got)
	}
}

// ── chat: local lane + argv ─────────────────────────────────────────────────--

func TestChatLocalLane(t *testing.T) {
	setTargetConf(t)
	defer stubCLI(embedded(map[string]any{
		"payloads": []any{map[string]any{"text": "Got it — noted."}},
		"meta": map[string]any{"stopReason": "stop",
			"systemPromptReport": map[string]any{"sessionKey": "agent:main:explicit:s1"}}}), "")()
	out, err := newOp(Chat, "chat", map[string]any{"message": "hi", "session_id": "s1"}).
		Process(map[string]any{"user_key": "uk"})
	if err != nil || out["lane"] != "local" || out["reply"] != "Got it — noted." {
		t.Fatalf("local chat: %v %v", out, err)
	}
	if out["ov_session_uri"] != SessionURI("s1", "main") {
		t.Errorf("embedded uri reconstruction must agree: %v", out["ov_session_uri"])
	}
	// a gateway-shaped envelope in local mode must fail
	defer stubCLI(gateway(map[string]any{
		"payloads": []any{map[string]any{"text": "ack"}}, "meta": gwMeta()}), "")()
	_, err = newOp(Chat, "chat", map[string]any{"message": "hi", "session_id": "s1"}).
		Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "local openclaw envelope") {
		t.Fatalf("gateway shape in local mode must fail, got %v", err)
	}
}

func TestChatArgvAndEnvUseLocalCLI(t *testing.T) {
	setTargetConf(t)
	var seen []string
	var env map[string]string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, envExtra map[string]string, _, _ int) root.CLIResult {
		seen = argv
		env = envExtra
		return root.CLIResult{ExitCode: 0, Stdout: embedded(map[string]any{
			"payloads": []any{map[string]any{"text": "ack"}}, "meta": map[string]any{"stopReason": "stop"}})}
	}
	defer func() { root.RunCLIContext = orig }()
	newOp(Chat, "chat", map[string]any{
		"message": "hi", "session_id": "s1",
		"openviking_url": "http://cfg.ov", "openviking_api_key": "cfg-key"}).
		Process(map[string]any{"user_key": "uk"})
	if !hasStr(seen, "--local") || hasStr(seen, "--agent") {
		t.Errorf("local argv must include --local, never --agent: %v", seen)
	}
	if !hasStr(seen, "--session-id") {
		t.Errorf("argv missing --session-id: %v", seen)
	}
	if env["OPENVIKING_URL"] != "http://cfg.ov" || env["OPENVIKING_BASE_URL"] != "http://cfg.ov" ||
		env["OPENVIKING_API_KEY"] != "cfg-key" {
		t.Errorf("explicit OpenViking env = %v", env)
	}
}

func TestChatAndCompactShareIsolatedRuntimeConfig(t *testing.T) {
	setTargetConf(t)
	stateDir := filepath.Join(t.TempDir(), "openclaw-state")
	pluginRoot := filepath.Join(t.TempDir(), "openclaw-plugin")
	if err := os.MkdirAll(pluginRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "openclaw.plugin.json"), []byte(`{"id":"openviking"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_OPENCLAW_PLUGIN_ROOT", pluginRoot)
	t.Setenv("OPENVIKING_LLM_BASE_URL", "https://llm.example.test/v1")
	t.Setenv("OPENVIKING_LLM_MODEL", "test-model")
	t.Setenv("OPENVIKING_LLM_API_KEY", "llm-secret")

	var calls []map[string]string
	var compactArgv []string
	gatewayStopped := false
	origGateway := startGatewayFn
	startGatewayFn = func(_ context.Context, _ map[string]string, gotStateDir string, gotTimeout time.Duration) (string, func() error, error) {
		if gotStateDir != stateDir {
			t.Fatalf("gateway state dir = %q, want %q", gotStateDir, stateDir)
		}
		if gotTimeout != 75*time.Second {
			t.Fatalf("gateway startup timeout = %s, want 75s", gotTimeout)
		}
		return "ws://127.0.0.1:43210", func() error { gatewayStopped = true; return nil }, nil
	}
	defer func() { startGatewayFn = origGateway }()
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, envExtra map[string]string, _, _ int) root.CLIResult {
		if hasStr(argv, "export-trajectory") {
			outputDir := filepath.Join(t.TempDir(), "trajectory")
			if err := os.MkdirAll(outputDir, 0o700); err != nil {
				t.Fatal(err)
			}
			branch := `{"entries":[{"role":"user","content":"hi"},{"role":"assistant","content":"ack"}]}`
			if err := os.WriteFile(filepath.Join(outputDir, "session-branch.json"), []byte(branch), 0o600); err != nil {
				t.Fatal(err)
			}
			summary, _ := json.Marshal(map[string]any{"outputDir": outputDir})
			return root.CLIResult{ExitCode: 0, Stdout: string(summary)}
		}
		calls = append(calls, envExtra)
		if hasStr(argv, "compact") {
			compactArgv = append([]string(nil), argv...)
			return root.CLIResult{ExitCode: 0, Stdout: `{"ok":true,"compacted":true}`}
		}
		return root.CLIResult{ExitCode: 0, Stdout: embedded(map[string]any{
			"payloads": []any{map[string]any{"text": "ack"}}, "meta": map[string]any{"stopReason": "stop"}})}
	}
	defer func() { root.RunCLIContext = orig }()

	chatOut, err := newOp(Chat, "chat", map[string]any{"message": "hi", "session_id": "s1", "state_dir": stateDir, "openviking_url": "http://cfg.ov", "openviking_api_key": "cfg-key", "auto_capture": false, "auto_recall": false}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("isolated chat: %v", err)
	}
	if transcript := root.AsString(chatOut["transcript"]); !strings.Contains(transcript, `"role":"user"`) || !strings.Contains(transcript, `"role":"assistant"`) {
		t.Fatalf("exported transcript = %q", transcript)
	}
	_, err = newOp(Compact, "compact", map[string]any{"state_dir": stateDir, "openviking_url": "http://cfg.ov", "openviking_api_key": "cfg-key", "auto_capture": false, "auto_recall": false, "compaction_timeout_seconds": 480, "gateway_start_timeout_seconds": 75}).Process(map[string]any{"session_key": "agent:main:explicit:s1"})
	if err != nil {
		t.Fatalf("isolated compact: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("runtime calls = %d", len(calls))
	}
	if !gatewayStopped || hasStr(compactArgv, "--url") {
		t.Fatalf("compact must stop the isolated gateway without putting its URL or credential on argv: argv=%v stopped=%v", compactArgv, gatewayStopped)
	}
	compactEnv := calls[len(calls)-1]
	if compactEnv["OPENCLAW_GATEWAY_URL"] != "ws://127.0.0.1:43210" || compactEnv["OPENCLAW_GATEWAY_TOKEN"] == "" {
		t.Fatalf("compact gateway environment was not isolated: url=%q token_set=%v", compactEnv["OPENCLAW_GATEWAY_URL"], compactEnv["OPENCLAW_GATEWAY_TOKEN"] != "")
	}
	for i, env := range calls {
		for key, want := range map[string]string{
			"OPENCLAW_STATE_DIR":   stateDir,
			"HOME":                 filepath.Join(stateDir, "home"),
			"OPENCLAW_CONFIG_PATH": filepath.Join(stateDir, "openclaw.json"),
		} {
			if env[key] != want {
				t.Fatalf("call %d env[%s]=%q want %q; env=%v", i, key, env[key], want, env)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "openclaw.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{pluginRoot, `"contextEngine": "openviking"`, `"primary": "ovtest/test-model"`, `${OPENVIKING_LLM_API_KEY}`, `"bundledDiscovery": "allowlist"`, `"timeoutSeconds": 480`} {
		if !strings.Contains(text, want) {
			t.Fatalf("isolated config missing %q: %s", want, text)
		}
	}
	for _, want := range []string{`"autoCapture": false`, `"autoRecall": false`, `"autoRecallTimeoutMs": 120000`} {
		if !strings.Contains(text, want) {
			t.Fatalf("isolated config did not honor %q: %s", want, text)
		}
	}
	if strings.Contains(text, "llm-secret") || strings.Contains(text, "cfg-key") {
		t.Fatalf("isolated config persisted a credential: %s", text)
	}
}

func TestChatOpenVikingEnvDefaultsToConfiguredUser(t *testing.T) {
	setTargetConf(t)
	var env map[string]string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, envExtra map[string]string, _, _ int) root.CLIResult {
		env = envExtra
		return root.CLIResult{ExitCode: 0, Stdout: embedded(map[string]any{
			"payloads": []any{map[string]any{"text": "ack"}}, "meta": map[string]any{"stopReason": "stop"}})}
	}
	defer func() { root.RunCLIContext = orig }()
	newOp(Chat, "chat", map[string]any{"message": "hi", "session_id": "s1"}).
		Process(map[string]any{})
	if env["OPENVIKING_URL"] != "http://ov.local" || env["OPENVIKING_BASE_URL"] != "http://ov.local" ||
		env["OPENVIKING_API_KEY"] != "conf-key" {
		t.Errorf("default OpenViking env = %v", env)
	}
}

func TestChatRejectsNonZeroExitEvenWithJSON(t *testing.T) {
	setTargetConf(t)
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, _ []string, _ map[string]string, _, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 2, Stdout: embedded(map[string]any{
			"payloads": []any{map[string]any{"text": "ack"}},
			"meta":     map[string]any{"stopReason": "stop"},
		}), Stderr: "openclaw failed"}
	}
	defer func() { root.RunCLIContext = orig }()

	_, err := newOp(Chat, "chat", map[string]any{"message": "hi", "session_id": "s1"}).
		Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "openclaw failed") {
		t.Fatalf("non-zero openclaw exit should fail before JSON acceptance, got %v", err)
	}
}

func TestSetupPassesOpenVikingKeyThroughEnv(t *testing.T) {
	setTargetConf(t)
	var seen []string
	var env map[string]string
	orig := root.RunCLIContext
	root.RunCLIContext = func(_ context.Context, argv []string, envExtra map[string]string, _, _ int) root.CLIResult {
		seen = argv
		env = envExtra
		return root.CLIResult{ExitCode: 0, Stdout: embedded(map[string]any{
			"success":  true,
			"health":   map[string]any{"ok": true},
			"keyProbe": map[string]any{"keyType": "user_key"},
		})}
	}
	defer func() { root.RunCLIContext = orig }()

	out, err := newOp(Setup, "setup", nil).Process(map[string]any{"user_key": "secret-user-key"})
	if err != nil || out["ok"] != true {
		t.Fatalf("setup: out=%v err=%v", out, err)
	}
	if hasStr(seen, "--api-key") || strings.Contains(strings.Join(seen, " "), "secret-user-key") {
		t.Fatalf("setup argv leaked api key: %v", seen)
	}
	if env["OPENVIKING_API_KEY"] != "secret-user-key" {
		t.Fatalf("setup env OPENVIKING_API_KEY = %q", env["OPENVIKING_API_KEY"])
	}
}

// ── openclaw_status preflight ───────────────────────────────────────────────--

func TestOpenclawStatusGates(t *testing.T) {
	defer stubCLI(embedded(map[string]any{"configured": true,
		"health": map[string]any{"ok": true}, "keyProbe": map[string]any{"keyType": "user_key"}}), "")()
	out, err := newOp(Status, "status", nil).Process(nil)
	if err != nil || out["ok"] != true {
		t.Fatalf("healthy status: %v %v", out, err)
	}

	restore := stubCLI(embedded(map[string]any{"configured": false}), "")
	_, err = newOp(Status, "status", nil).Process(nil)
	restore()
	if err == nil || !contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured must fail, got %v", err)
	}

	defer stubCLI(embedded(map[string]any{"configured": true,
		"health":   map[string]any{"ok": true},
		"keyProbe": map[string]any{"keyType": "unknown", "detail": "HTTP 401"}}), "")()
	_, err = newOp(Status, "status", nil).Process(nil)
	if err == nil || !contains(err.Error(), "unusable") {
		t.Fatalf("401 key probe must fail, got %v", err)
	}
}

// ── ov_session transcript reader ────────────────────────────────────────────--

func TestOvSessionGates(t *testing.T) {
	setTargetConf(t)
	var seen []string
	restore := stubOv(func(args []string, _ string, _ int) root.CLIResult {
		seen = args
		return root.CLIResult{ExitCode: 0, Stdout: "user: codename is Basalt-ab12, launches March 14th"}
	})
	wired := "viking://session/12345678-1234-1234-1234-123456789abc/messages.jsonl"
	out, err := newOp(Session, "verify", map[string]any{
		"session_id": "ignored", "expect": []string{"basalt-ab12", "march 14"}}).Process(
		map[string]any{"uri": wired})
	restore()
	if err != nil || !contains(root.AsString(out["transcript"]), "Basalt-ab12") {
		t.Fatalf("transcript with fact passes: %v %v", out, err)
	}
	if !equalStrs(seen, []string{"read", wired}) {
		t.Errorf("wired uri must be read verbatim: %v", seen)
	}

	// missing fact -> gate fail
	restore = stubOv(func(_ []string, _ string, _ int) root.CLIResult {
		return root.CLIResult{ExitCode: 0, Stdout: "user: hello there"}
	})
	_, err = newOp(Session, "verify", map[string]any{
		"session_id": "s1", "expect": []string{"basalt-ab12"}}).Process(nil)
	restore()
	if err == nil || !contains(err.Error(), "missing") {
		t.Fatalf("absent fact must fail, got %v", err)
	}

	// empty expect -> config error (the only content gate must not be vacuous)
	restore = stubOv(func(_ []string, _ string, _ int) root.CLIResult { return root.CLIResult{ExitCode: 0, Stdout: "x"} })
	_, err = newOp(Session, "verify", map[string]any{
		"session_id": "s1", "expect": []string{}, "gate": "soft"}).Process(nil)
	restore()
	var ce *root.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("empty expect must be a ConfigError even under gate:soft, got %v", err)
	}
}

func TestOpenClawToolEvidencePairsSuccessfulResults(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","toolCallId":"c1","toolName":"memory_store","arguments":{"text":"prompt-only marker"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"c1","toolName":"memory_store","content":[{"type":"text","text":"Stored viking://user/memories/x"}],"isError":false}}`,
	}, "\n")
	successful, observed, text := parseOpenClawToolEvidence(jsonl)
	if !successful.Contains("memory_store") || !observed.Contains("memory_store") || !strings.Contains(text, "viking://user/memories/x") {
		t.Fatalf("successful=%v observed=%v text=%q", successful, observed, text)
	}
}

func TestOpenClawToolEvidenceRejectsBusinessError(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","toolCallId":"c1","toolName":"memory_store","arguments":{}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"c1","toolName":"memory_store","content":[{"type":"text","text":"Error: unauthorized"}],"isError":false}}`,
	}, "\n")
	successful, observed, _ := parseOpenClawToolEvidence(jsonl)
	if successful.Contains("memory_store") || !observed.Contains("memory_store") {
		t.Fatalf("business error must be observed but not successful: successful=%v observed=%v", successful, observed)
	}
}

func TestOpenClawToolEvidenceAcceptsPrettyExportAndCurrentCallShape(t *testing.T) {
	transcript := `{
  "type": "message",
  "message": {
    "role": "assistant",
    "content": [{"type":"toolCall","id":"call-1","name":"memory_store","arguments":{"text":"marker"}}]
  }
}
{
  "type": "message",
  "message": {
    "role": "toolResult",
    "toolCallId": "call-1",
    "toolName": "memory_store",
    "content": [{"type":"text","text":"stored"}],
    "details": {"status":"completed"},
    "isError": false
  }
}`
	successful, observed, _ := parseOpenClawToolEvidence(transcript)
	if !successful.Contains("memory_store") || !observed.Contains("memory_store") {
		t.Fatalf("successful=%v observed=%v", successful, observed)
	}
}

func TestOpenClawToolEvidenceRejectsUnmatchedResult(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"memory_store","arguments":{}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"different-call","toolName":"memory_store","content":[{"type":"text","text":"stored"}],"isError":false}}`,
	}, "\n")
	successful, observed, _ := parseOpenClawToolEvidence(transcript)
	if successful.Contains("memory_store") || !observed.Contains("memory_store") {
		t.Fatalf("unmatched result must not pass: successful=%v observed=%v", successful, observed)
	}
}

func TestOpenClawCompactRequiresExplicitSuccess(t *testing.T) {
	setTargetConf(t)
	defer stubCLI(`{"ok":true,"compacted":true}`, "")()
	out, err := newOp(Compact, "compact", nil).Process(map[string]any{"session_key": "agent:main:explicit:s1"})
	if err != nil || out["ok"] != true || out["compacted"] != true {
		t.Fatalf("compact out=%v err=%v", out, err)
	}

	restore := stubCLI(`{"ok":true,"compacted":false}`, "")
	_, err = newOp(Compact, "compact", nil).Process(map[string]any{"session_key": "agent:main:explicit:s1"})
	restore()
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("non-compacted result must fail, got %v", err)
	}
}
