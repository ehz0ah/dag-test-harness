package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// Pi is driven through its documented JSON-RPC mode. The OpenViking
// integration is a native Pi extension, so evidence comes from Pi lifecycle
// and tool events rather than MCP events.
var (
	Exec     = execOp()
	Evidence = evidenceOp()
	runRPC   = runPiRPC
)

func piBin() string {
	if value := strings.TrimSpace(os.Getenv("OV_TEST_PI_BIN")); value != "" {
		return value
	}
	return "pi"
}

func execOp() dag.Factory {
	return root.NewFactory(dag.Meta{
		Inputs: []string{"user_key", "after", "memory_uri", "resource_url", "archive_session_id", "case_id"},
		Outputs: []string{
			"reply", "jsonl", "ov_session_id", "jsonl_path", "project_dir", "state_dir",
			"agent_dir", "extension_dir", "session_file", "compacted",
		},
	}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			message := renderMessage(root.AsString(ctx.Config()["message"]), root.AsString(ctx.Config()["message_template"]), ctx.Config(), in)
			if strings.TrimSpace(message) == "" {
				return nil, ctx.ConfigErr("pi_exec requires a non-empty message")
			}
			projectDir, err := absoluteDir(root.FirstNonEmpty(root.AsString(ctx.Config()["project_dir"]), os.Getenv("OV_TEST_PI_PROJECT_DIR")), "ovtest-pi-project-")
			if err != nil {
				return nil, ctx.ConfigErr(err.Error())
			}
			stateDir := root.FirstNonEmpty(root.AsString(ctx.Config()["state_dir"]), os.Getenv("OV_TEST_PI_STATE_DIR"))
			if stateDir == "" {
				stateDir = filepath.Join(projectDir, ".ovtest-pi")
			}
			stateDir, err = filepath.Abs(stateDir)
			if err != nil {
				return nil, ctx.ConfigErr("resolve Pi state directory: " + err.Error())
			}
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				return nil, ctx.ConfigErr("create Pi state directory: " + err.Error())
			}

			env, agentDir, extensionDir, err := preparePiEnvironment(ctx.Name(), ctx.Config(), in["user_key"], projectDir, stateDir)
			if err != nil {
				return nil, err
			}
			jsonlPath := root.AsString(ctx.Config()["jsonl_path"])
			if jsonlPath == "" {
				jsonlPath = filepath.Join(stateDir, ctx.Name()+".jsonl")
			}
			jsonlPath, err = filepath.Abs(jsonlPath)
			if err != nil {
				return nil, ctx.ConfigErr("resolve Pi evidence path: " + err.Error())
			}

			model := root.FirstNonEmpty(root.AsString(ctx.Config()["model"]), os.Getenv("OV_TEST_PI_MODEL"))
			if model == "" {
				return nil, ctx.ConfigErr("Pi requires OV_TEST_PI_MODEL or config model")
			}
			argv := []string{
				piBin(), "--mode", "rpc", "--approve",
				"--no-extensions", "--extension", filepath.Join(extensionDir, "index.ts"),
				"--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files",
				"--session-dir", filepath.Join(stateDir, "sessions"),
				"--model", "ovtest/" + model,
			}
			if root.AsBool(ctx.Config()["disable_tools"]) {
				argv = append(argv, "--no-tools")
			}
			timeout := root.AsInt(ctx.Config()["timeout"], root.EnvInt("OV_TEST_PI_TIMEOUT", 900))
			result := runRPC(ctx.Context(), projectDir, argv, env, message, root.AsBool(ctx.Config()["compact_after"]), timeout)
			if err := os.WriteFile(jsonlPath, []byte(result.Stdout), 0o600); err != nil {
				return nil, ctx.ConfigErr("write Pi JSONL evidence: " + err.Error())
			}
			if err := ctx.OK(result); err != nil {
				return nil, err
			}
			evidence := parsePiRun(result.Stdout)
			if evidence.ExtensionError != "" {
				return nil, ctx.GateErr("Pi extension error: " + evidence.ExtensionError)
			}
			if evidence.Reply == "" {
				return nil, ctx.GateErr("Pi produced no final assistant reply")
			}
			if root.AsBool(ctx.Config()["compact_after"]) && !evidence.Compacted {
				return nil, ctx.GateErr("Pi RPC compaction did not complete successfully")
			}

			out := root.CLIFields(result)
			out["reply"], out["jsonl"], out["jsonl_path"] = evidence.Reply, result.Stdout, jsonlPath
			out["ov_session_id"] = piOVSessionID(evidence.SessionID)
			out["project_dir"], out["state_dir"], out["agent_dir"], out["extension_dir"] = projectDir, stateDir, agentDir, extensionDir
			out["session_file"], out["compacted"] = evidence.SessionFile, evidence.Compacted
			return out, nil
		}
	})
}

func evidenceOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"jsonl", "reply", "after"}, Outputs: []string{"ok", "tools"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			run := parsePiRun(root.AsString(in["jsonl"]))
			completed := completedPiTools(root.AsString(in["jsonl"]))
			if root.AsBool(ctx.Config()["forbid_any_tool"]) {
				observed := observedPiTools(root.AsString(in["jsonl"]))
				if len(observed) > 0 {
					tools := make([]string, 0, len(observed))
					for name := range observed {
						tools = append(tools, name)
					}
					sort.Strings(tools)
					return nil, ctx.GateErr(fmt.Sprintf("Pi evidence contained forbidden tool call(s) %v", tools))
				}
			}
			missing := []string{}
			for _, name := range root.AsStrings(ctx.Config()["expect_tools"]) {
				if !completed.Contains(name) {
					missing = append(missing, name)
				}
			}
			forbidden := []string{}
			for _, name := range root.AsStrings(ctx.Config()["forbid_tools"]) {
				if completed.Contains(name) {
					forbidden = append(forbidden, name)
				}
			}
			if len(missing) > 0 || len(forbidden) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Pi tool evidence missing=%v forbidden=%v", missing, forbidden))
			}
			if root.AsBool(ctx.Config()["expect_compaction"]) && !run.Compacted {
				return nil, ctx.GateErr("Pi JSONL omitted a successful compaction lifecycle")
			}
			tools := make([]string, 0, len(completed))
			for name := range completed {
				tools = append(tools, name)
			}
			sort.Strings(tools)
			return map[string]any{"ok": true, "tools": tools}, nil
		}
	})
}

func preparePiEnvironment(node string, cfg map[string]any, userKey any, projectDir, stateDir string) (map[string]string, string, string, error) {
	conf, _ := root.ReadCLIConf(root.UserBaseConf())
	endpoint := root.FirstNonEmpty(root.AsString(cfg["openviking_url"]), root.FirstNonEmpty(os.Getenv("OV_TEST_PI_OPENVIKING_URL"), root.AsString(conf["url"])))
	key := root.FirstNonEmpty(root.AsString(cfg["openviking_api_key"]), root.FirstNonEmpty(os.Getenv("OV_TEST_PI_OPENVIKING_API_KEY"), root.AsString(userKey)))
	if endpoint == "" || key == "" {
		return nil, "", "", root.ConfigErrorFor(node, "Pi requires an OpenViking URL and API key")
	}
	llmURL := root.FirstNonEmpty(root.AsString(cfg["llm_base_url"]), os.Getenv("OV_TEST_PI_LLM_BASE_URL"))
	llmKey := root.FirstNonEmpty(root.AsString(cfg["llm_api_key"]), os.Getenv("OV_TEST_PI_LLM_API_KEY"))
	model := root.FirstNonEmpty(root.AsString(cfg["model"]), os.Getenv("OV_TEST_PI_MODEL"))
	protocol := root.FirstNonEmpty(root.AsString(cfg["llm_protocol"]), os.Getenv("OV_TEST_PI_LLM_PROTOCOL"))
	if llmURL == "" || llmKey == "" || model == "" {
		return nil, "", "", root.ConfigErrorFor(node, "Pi requires LLM base URL, API key, and model")
	}
	if protocol == "" {
		protocol = "openai-completions"
	}
	if protocol != "openai-completions" && protocol != "openai-responses" {
		return nil, "", "", root.ConfigErrorFor(node, "Pi LLM protocol must be openai-completions or openai-responses")
	}

	pluginRoot := root.FirstNonEmpty(root.AsString(cfg["extension_root"]), os.Getenv("OV_TEST_PI_EXTENSION_ROOT"))
	if pluginRoot == "" {
		pluginRoot = filepath.Join(os.Getenv("OV_TEST_OPENVIKING_REPO"), "examples", "pi-coding-agent-extension")
	}
	if pluginRoot == "" {
		return nil, "", "", root.ConfigErrorFor(node, "Pi OpenViking extension root is unavailable")
	}
	extensionDir := filepath.Join(stateDir, "extension")
	if err := copyTree(pluginRoot, extensionDir); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "install isolated Pi extension: "+err.Error())
	}
	pluginConfig := map[string]any{
		"enabled":               true,
		"syncTurns":             root.AsBool(cfg["auto_capture"]),
		"captureMode":           "semantic",
		"commitTokenThreshold":  root.AsInt(cfg["commit_token_threshold"], 1000),
		"commitKeepRecentCount": root.AsInt(cfg["commit_keep_recent_count"], 0),
		"scoreThreshold":        root.FirstNonEmpty(root.AsString(cfg["score_threshold"]), "0"),
		"minQueryLength":        3,
		"takeover": map[string]any{
			"enabled":         root.AsBool(cfg["takeover"]),
			"tokenThreshold":  root.AsInt(cfg["takeover_token_threshold"], 1),
			"keepRecentTurns": root.AsInt(cfg["takeover_keep_recent_turns"], 0),
			"overviewBudget":  3000,
			"overviewPollMs":  1000,
			"overviewPollMax": 60,
		},
	}
	if err := writeJSON(filepath.Join(extensionDir, "config.json"), pluginConfig); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "write Pi extension config: "+err.Error())
	}

	agentDir := filepath.Join(stateDir, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "create Pi agent directory: "+err.Error())
	}
	models := map[string]any{"providers": map[string]any{"ovtest": map[string]any{
		"baseUrl": strings.TrimRight(llmURL, "/"), "api": protocol, "apiKey": "$OV_TEST_PI_LLM_API_KEY",
		"models": []any{map[string]any{
			"id": model, "name": "ovtest release model", "reasoning": false,
			"input": []string{"text"}, "contextWindow": 128000, "maxTokens": 8192,
		}},
	}}}
	if err := writeJSON(filepath.Join(agentDir, "models.json"), models); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "write Pi model config: "+err.Error())
	}
	settings := map[string]any{"compaction": map[string]any{
		"keepRecentTokens": root.AsInt(cfg["compaction_keep_recent_tokens"], 20000),
	}}
	if err := writeJSON(filepath.Join(agentDir, "settings.json"), settings); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "write Pi settings: "+err.Error())
	}

	secretDir := root.SecretStateDir("pi", stateDir)
	if secretDir == "" {
		secretDir = filepath.Join(stateDir, "secrets")
	}
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "create Pi secret directory: "+err.Error())
	}
	home := filepath.Join(stateDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, "", "", root.ConfigErrorFor(node, "create Pi home: "+err.Error())
	}
	env := map[string]string{
		"HOME":                            home,
		"PI_CODING_AGENT_DIR":             agentDir,
		"PI_CODING_AGENT_SESSION_DIR":     filepath.Join(stateDir, "sessions"),
		"OV_TEST_PI_LLM_API_KEY":          llmKey,
		"OPENVIKING_URL":                  strings.TrimRight(endpoint, "/"),
		"OPENVIKING_API_KEY":              key,
		"OPENVIKING_AUTH_MODE":            "api_key",
		"OPENVIKING_ACCOUNT":              root.FirstNonEmpty(root.AsString(cfg["openviking_account"]), root.AsString(conf["account"])),
		"OPENVIKING_USER":                 root.FirstNonEmpty(root.AsString(cfg["openviking_user"]), root.AsString(conf["user"])),
		"OPENVIKING_PEER_ID":              root.FirstNonEmpty(root.AsString(cfg["openviking_peer_id"]), "pi-ovtest"),
		"OPENVIKING_RECALL_PEER_SCOPE":    root.FirstNonEmpty(root.AsString(cfg["recall_peer_scope"]), "all"),
		"OPENVIKING_WORKSPACE_PEER":       "0",
		"OPENVIKING_PENDING_DIR":          filepath.Join(stateDir, "pending"),
		"OPENVIKING_QUEUE_SCOPE_KEY_FILE": root.QueueScopeKeyFile("pi", stateDir),
		"OPENVIKING_DEBUG_LOG":            filepath.Join(stateDir, "openviking-pi.log"),
		"NO_COLOR":                        "1",
	}
	return env, agentDir, extensionDir, nil
}

func runPiRPC(parent context.Context, cwd string, argv []string, env map[string]string, message string, compact bool, timeoutSeconds int) root.CLIResult {
	started := time.Now()
	ctx := parent
	var cancel context.CancelFunc
	if timeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
		defer cancel()
	}
	result := root.CLIResult{Cmd: strings.Join(argv, " ")}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	preparePiCommand(cmd)
	cmd.Cancel = func() error { return cancelPiCommand(cmd) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = cwd
	cmd.Env = mergeEnv(os.Environ(), env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		result.ExitCode, result.Stderr = 127, err.Error()
		return result
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.ExitCode, result.Stderr = 127, err.Error()
		return result
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		result.ExitCode, result.Stderr = 127, err.Error()
		return result
	}
	encoder := json.NewEncoder(stdin)
	_ = encoder.Encode(map[string]any{"id": "state", "type": "get_state"})
	_ = encoder.Encode(map[string]any{"id": "prompt", "type": "prompt", "message": message})

	var output bytes.Buffer
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	compactSent := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		output.Write(line)
		output.WriteByte('\n')
		var event map[string]any
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event["type"] == "agent_settled" && !compactSent {
			if compact {
				compactSent = true
				_ = encoder.Encode(map[string]any{"id": "compact", "type": "compact"})
			} else {
				_ = stdin.Close()
			}
		}
		if event["type"] == "response" && event["command"] == "compact" {
			_ = stdin.Close()
		}
	}
	if scanErr := scanner.Err(); scanErr != nil && ctx.Err() == nil {
		stderr.WriteString("\n" + scanErr.Error())
	}
	waitErr := cmd.Wait()
	result.Stdout, result.Stderr = output.String(), strings.TrimSpace(stderr.String())
	result.DurationS = float64(time.Since(started).Milliseconds()) / 1000
	if ctx.Err() != nil {
		result.ExitCode = 124
		result.Stderr = strings.TrimSpace(result.Stderr + "\n" + ctx.Err().Error())
	} else if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = 127
			result.Stderr = strings.TrimSpace(result.Stderr + "\n" + waitErr.Error())
		}
	}
	return result
}

type piRun struct {
	Reply, SessionID, SessionFile, ExtensionError string
	Compacted                                     bool
}

func parsePiRun(jsonl string) piRun {
	var run piRun
	compactResponse, compactEvent := false, false
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch root.AsString(event["type"]) {
		case "response":
			command := root.AsString(event["command"])
			if command == "get_state" && event["success"] == true {
				data, _ := event["data"].(map[string]any)
				run.SessionID = root.AsString(data["sessionId"])
				run.SessionFile = root.AsString(data["sessionFile"])
			}
			if command == "compact" && event["success"] == true {
				compactResponse = true
			}
		case "message_end":
			if text := assistantText(event["message"]); text != "" {
				run.Reply = text
			}
		case "agent_end":
			for _, message := range anySlice(event["messages"]) {
				if text := assistantText(message); text != "" {
					run.Reply = text
				}
			}
		case "compaction_end":
			compactEvent = event["aborted"] != true && event["result"] != nil
		case "extension_error":
			run.ExtensionError = root.AsString(event["error"])
		}
	}
	run.Compacted = compactResponse && compactEvent
	return run
}

func completedPiTools(jsonl string) sharedevidence.ToolSet {
	tools := sharedevidence.ToolSet{}
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event["type"] != "tool_execution_end" || event["isError"] == true {
			continue
		}
		name := root.AsString(event["toolName"])
		if strings.HasPrefix(strings.ToLower(name), "viking_") && !sharedevidence.HasBusinessError(event["result"]) {
			tools.Add(name)
		}
	}
	return tools
}

func observedPiTools(jsonl string) sharedevidence.ToolSet {
	tools := sharedevidence.ToolSet{}
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil || !strings.HasPrefix(root.AsString(event["type"]), "tool_execution_") {
			continue
		}
		if name := root.AsString(event["toolName"]); name != "" {
			tools.Add(name)
		}
	}
	return tools
}

func assistantText(value any) string {
	message, _ := value.(map[string]any)
	if root.AsString(message["role"]) != "assistant" {
		return ""
	}
	if text := strings.TrimSpace(root.AsString(message["content"])); text != "" {
		return text
	}
	var parts []string
	for _, partValue := range anySlice(message["content"]) {
		part, _ := partValue.(map[string]any)
		if root.AsString(part["type"]) == "text" {
			parts = append(parts, root.AsString(part["text"]))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func piOVSessionID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	return "pi-" + safeID(sessionID)
}

func safeID(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

func renderMessage(message, template string, cfg map[string]any, in map[string]any) string {
	if template == "" {
		return message
	}
	for key, value := range cfg {
		template = strings.ReplaceAll(template, "{{"+key+"}}", fmt.Sprint(value))
	}
	for key, value := range in {
		template = strings.ReplaceAll(template, "{{"+key+"}}", fmt.Sprint(value))
	}
	return template
}

func absoluteDir(value, prefix string) (string, error) {
	var err error
	if value == "" {
		value, err = os.MkdirTemp("", prefix)
		if err != nil {
			return "", err
		}
	}
	value, err = filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(value, 0o700); err != nil {
		return "", err
	}
	return value, nil
}

func copyTree(source, destination string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported extension entry %s", path)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0o700|0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		return errors.Join(copyErr, closeErr)
	})
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := extra[key]; !replaced {
			out = append(out, item)
		}
	}
	for key, value := range extra {
		out = append(out, key+"="+value)
	}
	return out
}
