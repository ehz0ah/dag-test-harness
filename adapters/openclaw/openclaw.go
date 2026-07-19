package openclaw

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// openclaw: drive the local `openclaw agent --local` CLI plus its OpenViking
// context-engine plugin. ov_session reads back the transcript the plugin wrote.

var ovUUID = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var (
	Setup              = setupOp()
	Status             = statusOp()
	Chat               = chatOp()
	Session            = sessionOp()
	Evidence           = evidenceOp()
	Compact            = compactOp()
	exportTranscriptFn = exportOpenClawTranscript
	startGatewayFn     = startIsolatedOpenClawGateway
)

func openclawBin() string {
	if value := strings.TrimSpace(os.Getenv("OV_TEST_OPENCLAW_BIN")); value != "" {
		return value
	}
	return "openclaw"
}

// parseOpenclawJSON returns the LAST complete top-level JSON object in out
// (openclaw --json prints human warning lines and sometimes several objects).
func parseOpenclawJSON(out string) map[string]any {
	idx := 0
	last := map[string]any{}
	for idx < len(out) {
		rel := strings.IndexByte(out[idx:], '{')
		if rel < 0 {
			break
		}
		b := idx + rel
		dec := json.NewDecoder(strings.NewReader(out[b:]))
		var obj any
		if err := dec.Decode(&obj); err == nil {
			if m, ok := obj.(map[string]any); ok {
				last = m
			}
			idx = b + int(dec.InputOffset())
		} else {
			idx = b + 1
		}
	}
	return last
}

// parseOpenclawCLIJSON keeps the command's JSON output authoritative. Plugin
// diagnostics are written to stderr and may themselves contain JSON objects;
// appending stderr to stdout can therefore replace a valid command envelope
// with an unrelated diagnostic object.
func parseOpenclawCLIJSON(r root.CLIResult) map[string]any {
	if result := parseOpenclawJSON(r.Stdout); len(result) > 0 {
		return result
	}
	return parseOpenclawJSON(r.Stderr)
}

// walkFind returns the first value for key anywhere in a nested map/list (DFS).
func walkFind(obj any, key string) any {
	switch x := obj.(type) {
	case map[string]any:
		if v, ok := x[key]; ok {
			return v
		}
		for _, v := range x {
			if r := walkFind(v, key); r != nil {
				return r
			}
		}
	case []any:
		for _, v := range x {
			if r := walkFind(v, key); r != nil {
				return r
			}
		}
	}
	return nil
}

// openclawReply concatenates the agent reply text from a result envelope's payloads.
func openclawReply(env map[string]any) string {
	payloads, _ := env["payloads"].([]any)
	parts := make([]string, 0, len(payloads))
	for _, p := range payloads {
		if m, ok := p.(map[string]any); ok {
			parts = append(parts, root.AsString(m["text"]))
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// ovURIFor is the OV URI the plugin stores a transcript under: a UUID session id
// passes through verbatim, anything else hashes the session KEY.
func ovURIFor(sessionID, sessionKey string) string {
	if sessionID != "" && ovUUID.MatchString(sessionID) {
		return "viking://session/" + strings.ToLower(sessionID) + "/messages.jsonl"
	}
	h := sha256.Sum256([]byte(sessionKey))
	return "viking://session/" + hex.EncodeToString(h[:]) + "/messages.jsonl"
}

// SessionURI reconstructs the transcript URI from a configured local session id.
func SessionURI(sessionID, agent string) string {
	if agent == "" {
		agent = "main"
	}
	return ovURIFor(sessionID, "agent:"+agent+":explicit:"+sessionID)
}

func localOpenClawEnv(node string, cfg map[string]any, userKey any) (map[string]string, error) {
	url := root.FirstNonEmpty(root.AsString(cfg["openviking_url"]), os.Getenv("OV_TEST_OPENCLAW_OPENVIKING_URL"))
	if url == "" {
		resolved, err := root.TargetURL()
		if err != nil {
			return nil, root.ConfigErrorFor(node, "could not resolve OpenViking url from ovcli.conf: "+err.Error())
		}
		url = resolved
	}
	key := root.FirstNonEmpty(root.AsString(cfg["openviking_api_key"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCLAW_OPENVIKING_API_KEY"), root.AsString(userKey)))
	if key == "" {
		resolved, err := root.TargetAPIKey()
		if err != nil {
			return nil, root.ConfigErrorFor(node, "could not resolve OpenViking api_key from ovcli.conf: "+err.Error())
		}
		key = resolved
	}
	env := map[string]string{}
	if url != "" {
		env["OPENVIKING_URL"] = url
		env["OPENVIKING_BASE_URL"] = url
	}
	if key != "" {
		env["OPENVIKING_API_KEY"] = key
	}
	return env, nil
}

func isolatedOpenClawEnv(node string, cfg map[string]any, userKey any) (map[string]string, string, string, error) {
	env, err := localOpenClawEnv(node, cfg, userKey)
	if err != nil {
		return nil, "", "", err
	}
	stateDir := root.FirstNonEmpty(root.AsString(cfg["state_dir"]), os.Getenv("OV_TEST_OPENCLAW_STATE_DIR"))
	if stateDir == "" {
		return env, "", "", nil
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "could not resolve OpenClaw state dir: "+err.Error())
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "could not create OpenClaw state dir: "+err.Error())
	}
	home := filepath.Join(stateDir, "home")
	for _, dir := range []string{home, filepath.Join(stateDir, "xdg-config"), filepath.Join(stateDir, "xdg-data"), filepath.Join(stateDir, "xdg-cache"), filepath.Join(stateDir, "workspace")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", "", root.ConfigErrorFor(node, "could not create isolated OpenClaw directory: "+err.Error())
		}
	}
	configPath := filepath.Join(stateDir, "openclaw.json")
	if err := writeIsolatedOpenClawConfig(node, configPath, stateDir, cfg); err != nil {
		return nil, "", "", err
	}
	env["OPENCLAW_STATE_DIR"] = stateDir
	env["OPENCLAW_HOME"] = home
	env["HOME"] = home
	env["XDG_CONFIG_HOME"] = filepath.Join(stateDir, "xdg-config")
	env["XDG_DATA_HOME"] = filepath.Join(stateDir, "xdg-data")
	env["XDG_CACHE_HOME"] = filepath.Join(stateDir, "xdg-cache")
	env["OPENCLAW_CONFIG_PATH"] = configPath
	if key := strings.TrimSpace(os.Getenv("OPENVIKING_LLM_API_KEY")); key != "" {
		env["OPENVIKING_LLM_API_KEY"] = key
	}
	return env, stateDir, configPath, nil
}

func writeIsolatedOpenClawConfig(node, configPath, stateDir string, cfg map[string]any) error {
	template := root.FirstNonEmpty(root.AsString(cfg["openclaw_config_template"]), os.Getenv("OV_TEST_OPENCLAW_CONFIG_TEMPLATE"))
	if template == "" {
		template = root.FirstNonEmpty(root.AsString(cfg["config_path"]), os.Getenv("OV_TEST_OPENCLAW_CONFIG_PATH"))
	}
	config := map[string]any{}
	if template != "" {
		raw, err := os.ReadFile(template)
		if err != nil {
			return root.ConfigErrorFor(node, "could not read OpenClaw config template: "+err.Error())
		}
		var source map[string]any
		if err := json.Unmarshal(raw, &source); err != nil {
			return root.ConfigErrorFor(node, "could not decode OpenClaw config template: "+err.Error())
		}
		if models, ok := source["models"]; ok {
			config["models"] = models
		}
		if agents, ok := source["agents"].(map[string]any); ok {
			if defaults, ok := agents["defaults"].(map[string]any); ok {
				clean := map[string]any{}
				for _, key := range []string{"model", "models", "timeoutSeconds"} {
					if value, exists := defaults[key]; exists {
						clean[key] = value
					}
				}
				clean["workspace"] = filepath.Join(stateDir, "workspace")
				config["agents"] = map[string]any{"defaults": clean}
			}
		}
	}
	if _, ok := config["models"]; !ok {
		baseURL := strings.TrimSpace(os.Getenv("OPENVIKING_LLM_BASE_URL"))
		model := strings.TrimSpace(os.Getenv("OPENVIKING_LLM_MODEL"))
		apiKey := strings.TrimSpace(os.Getenv("OPENVIKING_LLM_API_KEY"))
		if baseURL == "" || model == "" || apiKey == "" {
			return root.ConfigErrorFor(node, "isolated OpenClaw requires OV_TEST_OPENCLAW_CONFIG_TEMPLATE or OPENVIKING_LLM_BASE_URL, OPENVIKING_LLM_MODEL, and OPENVIKING_LLM_API_KEY")
		}
		providerID := root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCLAW_LLM_PROVIDER"), "ovtest")
		api := root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCLAW_LLM_API"), "openai-completions")
		config["models"] = map[string]any{
			"mode": "replace",
			"providers": map[string]any{providerID: map[string]any{
				"baseUrl": baseURL, "apiKey": "${OPENVIKING_LLM_API_KEY}", "api": api,
				"models": []any{map[string]any{"id": model, "name": model, "input": []string{"text"}, "contextWindow": 128000, "maxTokens": 8192}},
			}},
		}
		config["agents"] = map[string]any{"defaults": map[string]any{
			"workspace": filepath.Join(stateDir, "workspace"),
			"model":     map[string]any{"primary": providerID + "/" + model},
			"models":    map[string]any{providerID + "/" + model: map[string]any{}},
		}}
	}
	if _, ok := config["agents"]; !ok {
		return root.ConfigErrorFor(node, "OpenClaw config template must define agents.defaults model selection")
	}
	agents, ok := config["agents"].(map[string]any)
	if !ok {
		return root.ConfigErrorFor(node, "OpenClaw config agents must be an object")
	}
	defaults, ok := agents["defaults"].(map[string]any)
	if !ok {
		return root.ConfigErrorFor(node, "OpenClaw config agents.defaults must be an object")
	}
	compactionTimeout := root.AsInt(cfg["compaction_timeout_seconds"], root.EnvInt("OV_TEST_OPENCLAW_COMPACTION_TIMEOUT_SECONDS", 540))
	if compactionTimeout <= 0 {
		return root.ConfigErrorFor(node, "OpenClaw compaction timeout must be positive")
	}
	defaults["compaction"] = map[string]any{"timeoutSeconds": compactionTimeout}
	pluginRoot := root.FirstNonEmpty(root.AsString(cfg["plugin_root"]), os.Getenv("OV_TEST_OPENCLAW_PLUGIN_ROOT"))
	if pluginRoot == "" {
		if repo := strings.TrimSpace(os.Getenv("OV_TEST_OPENVIKING_REPO")); repo != "" {
			pluginRoot = filepath.Join(repo, "examples", "openclaw-plugin")
		}
	}
	if pluginRoot == "" {
		return root.ConfigErrorFor(node, "isolated OpenClaw requires OV_TEST_OPENCLAW_PLUGIN_ROOT or OV_TEST_OPENVIKING_REPO")
	}
	pluginRoot, err := filepath.Abs(pluginRoot)
	if err != nil {
		return root.ConfigErrorFor(node, "could not resolve OpenClaw plugin root: "+err.Error())
	}
	if st, err := os.Stat(filepath.Join(pluginRoot, "openclaw.plugin.json")); err != nil || st.IsDir() {
		return root.ConfigErrorFor(node, "OpenClaw plugin manifest not found under "+pluginRoot)
	}
	config["plugins"] = map[string]any{
		"enabled": true, "allow": []string{"openviking"}, "bundledDiscovery": "allowlist",
		"load":  map[string]any{"paths": []string{pluginRoot}},
		"slots": map[string]any{"contextEngine": "openviking"},
		"entries": map[string]any{"openviking": map[string]any{
			"enabled": true,
			"config": map[string]any{
				"baseUrl": "${OPENVIKING_BASE_URL}", "apiKey": "${OPENVIKING_API_KEY}",
				"autoCapture": configBool(cfg, "auto_capture", true), "autoRecall": configBool(cfg, "auto_recall", true),
				"autoRecallTimeoutMs":       root.AsInt(cfg["auto_recall_timeout_ms"], root.EnvInt("OV_TEST_OPENCLAW_AUTO_RECALL_TIMEOUT_MS", 120_000)),
				"commitTokenThresholdRatio": configFloat(cfg, "commit_token_threshold_ratio", 0),
				"commitKeepRecentCount":     root.AsInt(cfg["commit_keep_recent_count"], 0),
				"enableAddResourceTool":     true, "enabledTools": "all",
			},
		}},
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return root.ConfigErrorFor(node, "could not encode isolated OpenClaw config: "+err.Error())
	}
	if err := os.WriteFile(configPath, append(raw, '\n'), 0o600); err != nil {
		return root.ConfigErrorFor(node, "could not write isolated OpenClaw config: "+err.Error())
	}
	return nil
}

func configFloat(cfg map[string]any, key string, fallback float64) float64 {
	switch value := cfg[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func configBool(cfg map[string]any, key string, fallback bool) bool {
	switch value := cfg[key].(type) {
	case bool:
		return value
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return fallback
}

func gateHealthKey(ctx *root.OpContext, env map[string]any, r root.CLIResult) (map[string]any, map[string]any, error) {
	health, _ := env["health"].(map[string]any)
	probe, _ := env["keyProbe"].(map[string]any)
	if !root.AsBool(health["ok"]) {
		return nil, nil, ctx.GateErr(fmt.Sprintf("server not healthy: %v", env["health"]))
	}
	if kt := root.AsString(probe["keyType"]); kt == "no_key" || kt == "unknown" {
		return nil, nil, ctx.GateErr(fmt.Sprintf("unusable api key: %v", env["keyProbe"]))
	}
	return health, probe, nil
}

func setupOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"ok"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			uk := root.AsString(in["user_key"])
			if uk == "" {
				return nil, ctx.GateErr("missing user_key (upstream create failed?)")
			}
			url, err := root.TargetURL()
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve OpenViking url from ovcli.conf: " + err.Error())
			}
			r := ctx.RunCLI([]string{openclawBin(), "openviking", "setup", "--base-url", url, "--json"},
				map[string]string{"OPENVIKING_API_KEY": uk}, 0, root.AsInt(ctx.Config()["timeout"], 60))
			env := parseOpenclawCLIJSON(r)
			if !root.AsBool(env["success"]) {
				return nil, ctx.GateErr("setup failed: " + root.FirstNonEmpty(root.AsString(env["error"]), root.ExitDetail(r)))
			}
			health, probe, err := gateHealthKey(ctx, env, r)
			if err != nil {
				return nil, err
			}
			out := root.CLIFields(r)
			out["ok"], out["health"], out["keyProbe"], out["slot"] = true, health, probe, env["slot"]
			return out, nil
		}
	})
}

func statusOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"after"}, Outputs: []string{"ok"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(map[string]any) (map[string]any, error) {
			r := ctx.RunCLI([]string{openclawBin(), "openviking", "status", "--json"}, nil, 0, root.AsInt(ctx.Config()["timeout"], 30))
			env := parseOpenclawCLIJSON(r)
			if !root.AsBool(env["configured"]) {
				return nil, ctx.GateErr(fmt.Sprintf("openviking plugin not configured: %v", firstNonNil(env, root.ExitDetail(r))))
			}
			health, probe, err := gateHealthKey(ctx, env, r)
			if err != nil {
				return nil, err
			}
			out := root.CLIFields(r)
			out["ok"], out["configured"], out["health"], out["keyProbe"] = true, true, health, probe
			return out, nil
		}
	})
}

func chatOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"user_key", "after", "memory_uri"},
		Outputs: []string{"reply", "ov_session_uri", "ov_session_id", "session_key", "session_file", "transcript", "transcript_path", "state_dir"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			message := root.AsString(ctx.Config()["message"])
			if tmpl := root.AsString(ctx.Config()["message_template"]); tmpl != "" {
				message = tmpl
				for key, value := range in {
					message = strings.ReplaceAll(message, "{{"+key+"}}", root.AsString(value))
				}
			}
			if strings.TrimSpace(message) == "" {
				return nil, ctx.ConfigErr("openclaw_chat requires non-empty message or message_template")
			}
			sessionID, err := ctx.NeedStr("session_id")
			if err != nil {
				return nil, err
			}
			agent := root.FirstNonEmpty(root.AsString(ctx.Config()["agent"]), "main")
			envExtra, stateDir, _, err := isolatedOpenClawEnv(ctx.Name(), ctx.Config(), in["user_key"])
			if err != nil {
				return nil, err
			}

			argv := []string{openclawBin(), "agent", "--local"}
			argv = append(argv, "--session-id", sessionID, "-m", message, "--json",
				"--timeout", strconv.Itoa(root.AsInt(ctx.Config()["agent_timeout"], 150)))
			r := ctx.RunCLI(argv, envExtra, 0, root.AsInt(ctx.Config()["timeout"], 200))
			if err := ctx.OK(r); err != nil {
				return nil, err
			}

			env := parseOpenclawCLIJSON(r)
			if _, ok := env["runId"]; ok {
				return nil, ctx.GateErr("expected a local openclaw envelope but got a gateway one")
			}
			res := env

			meta, _ := res["meta"].(map[string]any)
			stop := root.AsString(meta["stopReason"])
			reply := openclawReply(res)
			if stop != "stop" || reply == "" {
				return nil, ctx.GateErr(fmt.Sprintf(
					"agent turn did not complete (stopReason=%q, reply=%q); err=%s",
					stop, root.Truncate(reply, 120), root.Truncate(strings.TrimSpace(r.Stderr), 200)))
			}

			sessionKey := root.AsString(walkFind(env, "sessionKey"))
			if sessionKey == "" {
				sessionKey = "agent:" + agent + ":explicit:" + sessionID
			}
			agentMeta, _ := meta["agentMeta"].(map[string]any)
			sidUsed := root.AsString(agentMeta["sessionId"])
			sidForURI := sidUsed
			if sidForURI == "" {
				sidForURI = sessionID
			}
			uri := ovURIFor(sidForURI, sessionKey)
			var transcript, transcriptPath string
			if stateDir != "" {
				transcript, transcriptPath, err = exportTranscriptFn(ctx, envExtra, agent, sessionKey, stateDir)
				if err != nil {
					return nil, err
				}
			}
			out := root.CLIFields(r)
			out["reply"] = reply
			out["ov_session_uri"] = uri
			out["ov_session_id"] = strings.TrimSuffix(strings.TrimPrefix(uri, "viking://session/"), "/messages.jsonl")
			out["stop_reason"], out["session_key"], out["agent_session_id"], out["lane"] = stop, sessionKey, sidUsed, "local"
			out["session_file"], out["transcript"], out["transcript_path"], out["state_dir"] = root.AsString(agentMeta["sessionFile"]), transcript, transcriptPath, stateDir
			return out, nil
		}
	})
}

func exportOpenClawTranscript(ctx *root.OpContext, env map[string]string, agent, sessionKey, stateDir string) (string, string, error) {
	if stateDir == "" || sessionKey == "" {
		return "", "", ctx.GateErr("OpenClaw did not report isolated state and session identity for transcript export")
	}
	workspace := filepath.Join(stateDir, "workspace")
	name := "ovtest-" + strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(ctx.Name())
	r := ctx.RunCLI([]string{
		openclawBin(), "sessions", "export-trajectory",
		"--agent", agent, "--session-key", sessionKey,
		"--workspace", workspace, "--output", name, "--json",
	}, env, 0, 90)
	if r.ExitCode != 0 {
		return "", "", ctx.ConfigErr("could not export OpenClaw transcript: " + root.ExitDetail(r))
	}
	summary := parseOpenclawCLIJSON(r)
	outputDir := root.AsString(summary["outputDir"])
	if outputDir == "" {
		return "", "", ctx.GateErr("OpenClaw trajectory export did not report outputDir")
	}
	branchPath := filepath.Join(outputDir, "session-branch.json")
	raw, err := os.ReadFile(branchPath)
	if err != nil {
		return "", "", ctx.ConfigErr("could not read exported OpenClaw transcript: " + err.Error())
	}
	var branch struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &branch); err != nil {
		return "", "", ctx.ConfigErr("could not decode exported OpenClaw transcript: " + err.Error())
	}
	if len(branch.Entries) == 0 {
		return "", "", ctx.GateErr("exported OpenClaw transcript contained no entries")
	}
	var normalized strings.Builder
	for _, entry := range branch.Entries {
		normalized.Write(entry)
		normalized.WriteByte('\n')
	}
	return normalized.String(), branchPath, nil
}

func evidenceOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"transcript", "transcript_path", "session_file", "after"}, Outputs: []string{"ok", "text"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			raw := []byte(root.FirstNonEmpty(root.AsString(in["transcript"]), root.AsString(ctx.Config()["transcript"])))
			if len(raw) == 0 {
				path := root.FirstNonEmpty(
					root.FirstNonEmpty(root.AsString(in["transcript_path"]), root.AsString(ctx.Config()["transcript_path"])),
					root.FirstNonEmpty(root.AsString(in["session_file"]), root.AsString(ctx.Config()["session_file"])),
				)
				if path == "" {
					return nil, ctx.GateErr("OpenClaw did not report transcript evidence")
				}
				var err error
				raw, err = os.ReadFile(path)
				if err != nil {
					return nil, ctx.ConfigErr("could not read OpenClaw session transcript: " + err.Error())
				}
			}
			successful, observed, text := parseOpenClawToolEvidence(string(raw))
			var missing []string
			for _, tool := range root.AsStrings(ctx.Config()["expect_tools"]) {
				if !successful.Contains(tool) {
					missing = append(missing, tool)
				}
			}
			if len(missing) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("OpenClaw evidence missing successful tool result(s) %v", missing))
			}
			var forbidden []string
			for _, tool := range root.AsStrings(ctx.Config()["forbid_tools"]) {
				if observed.Contains(tool) {
					forbidden = append(forbidden, tool)
				}
			}
			if len(forbidden) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("OpenClaw evidence contained forbidden tool call(s) %v", forbidden))
			}
			return map[string]any{"ok": true, "text": text}, nil
		}
	})
}

func parseOpenClawToolEvidence(jsonl string) (sharedevidence.ToolSet, sharedevidence.ToolSet, string) {
	successful, observed := sharedevidence.ToolSet{}, sharedevidence.ToolSet{}
	calls := map[string]string{}
	var results []string
	decoder := json.NewDecoder(strings.NewReader(jsonl))
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			break
		}
		message, _ := event["message"].(map[string]any)
		if message == nil {
			message = event
		}
		collectOpenClawToolCalls(message["content"], observed, calls)
		role := strings.ToLower(root.AsString(message["role"]))
		if role == "toolresult" {
			tool := root.AsString(message["toolName"])
			callID := root.AsString(message["toolCallId"])
			if tool != "" && calls[callID] == tool && !sharedevidence.HasBusinessError(message) {
				successful.Add(tool)
				if raw, err := json.Marshal(message["content"]); err == nil {
					results = append(results, string(raw))
				}
			}
		}
	}
	return successful, observed, strings.Join(results, "\n")
}

func collectOpenClawToolCalls(v any, observed sharedevidence.ToolSet, calls map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if strings.EqualFold(root.AsString(x["type"]), "toolCall") {
			tool := root.FirstNonEmpty(root.AsString(x["name"]), root.AsString(x["toolName"]))
			observed.Add(tool)
			if callID := root.FirstNonEmpty(root.AsString(x["id"]), root.AsString(x["toolCallId"])); callID != "" && tool != "" {
				calls[callID] = tool
			}
		}
		for _, child := range x {
			collectOpenClawToolCalls(child, observed, calls)
		}
	case []any:
		for _, child := range x {
			collectOpenClawToolCalls(child, observed, calls)
		}
	}
}

func compactOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"session_key", "after"}, Outputs: []string{"ok", "compacted"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			key := root.AsString(in["session_key"])
			if key == "" {
				return nil, ctx.GateErr("openclaw compact requires a concrete session_key")
			}
			env, stateDir, _, err := isolatedOpenClawEnv(ctx.Name(), ctx.Config(), nil)
			if err != nil {
				return nil, err
			}
			argv := []string{openclawBin(), "sessions", "compact", key, "--json"}
			var stopGateway func() error
			if stateDir != "" {
				gatewayToken, err := randomGatewayToken()
				if err != nil {
					return nil, ctx.ConfigErr("could not generate isolated gateway credential: " + err.Error())
				}
				env["OPENCLAW_GATEWAY_TOKEN"] = gatewayToken
				gatewayStartTimeout := root.AsInt(ctx.Config()["gateway_start_timeout_seconds"], root.EnvInt("OV_TEST_OPENCLAW_GATEWAY_START_TIMEOUT_SECONDS", 120))
				if gatewayStartTimeout <= 0 {
					return nil, ctx.ConfigErr("OpenClaw gateway startup timeout must be positive")
				}
				gatewayURL, stop, err := startGatewayFn(ctx.Context(), env, stateDir, time.Duration(gatewayStartTimeout)*time.Second)
				if err != nil {
					return nil, ctx.GateErr("could not start isolated OpenClaw gateway: " + err.Error())
				}
				stopGateway = stop
				// A URL supplied on argv requires the credential on argv as well.
				// Environment-based URL + token keeps the run isolated without
				// leaking the ephemeral credential into process evidence.
				env["OPENCLAW_GATEWAY_URL"] = gatewayURL
			}
			r := ctx.RunCLI(argv, env, 0, root.AsInt(ctx.Config()["timeout"], 600))
			var cleanupErr error
			if stopGateway != nil {
				cleanupErr = stopGateway()
			}
			if runErr := ctx.OK(r); runErr != nil {
				if cleanupErr != nil {
					return nil, ctx.GateErr(runErr.Error() + "; isolated gateway cleanup failed: " + cleanupErr.Error())
				}
				return nil, runErr
			}
			if cleanupErr != nil {
				return nil, ctx.GateErr("isolated gateway cleanup failed: " + cleanupErr.Error())
			}
			result := parseOpenclawCLIJSON(r)
			if !root.AsBool(result["ok"]) || !root.AsBool(result["compacted"]) {
				return nil, ctx.GateErr(fmt.Sprintf("OpenClaw compaction did not complete: %v", result))
			}
			out := root.CLIFields(r)
			out["ok"], out["compacted"] = true, true
			return out, nil
		}
	})
}

func randomGatewayToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func startIsolatedOpenClawGateway(ctx context.Context, env map[string]string, stateDir string, startTimeout time.Duration) (string, func() error, error) {
	if strings.TrimSpace(env["OPENCLAW_GATEWAY_TOKEN"]) == "" {
		return "", nil, errors.New("isolated gateway token is missing")
	}
	if startTimeout <= 0 {
		return "", nil, errors.New("isolated gateway startup timeout must be positive")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("reserve loopback port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return "", nil, fmt.Errorf("release loopback port: %w", err)
	}
	logPath := filepath.Join(stateDir, "openclaw-gateway.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("open gateway log: %w", err)
	}
	cmd := exec.CommandContext(ctx, openclawBin(), "gateway", "run", "--allow-unconfigured", "--auth", "token", "--bind", "loopback", "--port", strconv.Itoa(port), "--ws-log", "compact")
	cmd.Env = mergedEnvironment(env)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", nil, fmt.Errorf("start gateway: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				stopErr = err
			}
			<-done
			if err := logFile.Close(); stopErr == nil && err != nil {
				stopErr = err
			}
		})
		return stopErr
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.NewTimer(startTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, dialErr := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			if raw, readErr := os.ReadFile(logPath); readErr == nil && strings.Contains(string(raw), "[gateway] ready") {
				return "ws://" + address, stop, nil
			}
		}
		select {
		case err := <-done:
			_ = logFile.Close()
			return "", nil, fmt.Errorf("gateway exited before readiness: %w (log: %s)", err, logPath)
		case <-ctx.Done():
			_ = stop()
			return "", nil, ctx.Err()
		case <-deadline.C:
			_ = stop()
			return "", nil, fmt.Errorf("gateway was not ready within %s (log: %s)", startTimeout, logPath)
		case <-ticker.C:
		}
	}
}

func mergedEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func sessionOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"user_key", "uri", "after"}, Outputs: []string{"transcript"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			uri := root.AsString(in["uri"])
			if uri == "" {
				sid, err := ctx.NeedStr("session_id")
				if err != nil {
					return nil, err
				}
				uri = SessionURI(sid, root.AsString(ctx.Config()["agent"]))
			}
			expectVal, err := ctx.Need("expect")
			if err != nil {
				return nil, err
			}
			expect := root.LowerAll(root.AsStrings(expectVal))
			if len(expect) == 0 {
				return nil, ctx.ConfigErr("ov_session requires a non-empty 'expect'")
			}
			settle, retry := root.AsInt(ctx.Config()["settle"], 0), root.AsInt(ctx.Config()["retry"], 0)
			conf, err := root.UserConf(ctx.Name(), in["user_key"])
			if err != nil {
				return nil, err
			}

			attempt := func(last bool) (map[string]any, error) {
				r := ctx.RunOv([]string{"read", uri}, conf, settle)
				if r.ExitCode != 0 {
					if last {
						return nil, ctx.GateErr("read " + uri + " failed: " + root.ExitDetail(r))
					}
					return nil, nil
				}
				low := strings.ToLower(r.Stdout)
				if root.ContainsAll(low, expect) {
					excerpt := strings.TrimSpace(r.Stdout)
					if len(excerpt) > 1200 {
						excerpt = excerpt[:1200] + "…"
					}
					out := root.CLIFields(r)
					out["transcript"], out["uri"] = excerpt, uri
					return out, nil
				}
				if last {
					var missing []string
					for _, s := range expect {
						if !strings.Contains(low, s) {
							missing = append(missing, s)
						}
					}
					return nil, ctx.GateErr(fmt.Sprintf(
						"session transcript missing %v after %d attempts (uri=%s)", missing, retry+1, uri))
				}
				return nil, nil
			}

			out, attempts, err := ctx.Poll(attempt, retry)
			if err != nil {
				return nil, err
			}
			out["attempts"] = attempts
			return out, nil
		}
	})
}

func firstNonNil(env map[string]any, fallback string) any {
	if len(env) > 0 {
		return env
	}
	return fallback
}
