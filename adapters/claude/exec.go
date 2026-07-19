package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// Claude adapter: drive the real non-interactive Claude Code CLI with the
// OpenViking Claude Code plugin. The harness owns process/env isolation only;
// memory capture, recall, and MCP calls must happen through Claude/plugin code.

var (
	Exec     = execOp()
	Evidence = evidenceOp()
)

func claudeBin() string {
	if v := os.Getenv("OV_TEST_CLAUDE_BIN"); v != "" {
		return v
	}
	return "claude"
}

func execOp() dag.Factory {
	return root.NewFactory(dag.Meta{
		Inputs:  []string{"user_key", "after", "resource_url", "resource_path", "memory_uri", "case_id"},
		Outputs: []string{"reply", "jsonl", "ov_session_id", "child_ov_session_id", "jsonl_path", "cwd", "state_dir", "cli_config_path", "debug_log_path"},
	}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			message, err := claudeMessage(ctx.Name(), ctx.Config(), in)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(message) == "" {
				return nil, ctx.ConfigErr("claude_exec requires non-empty message")
			}

			cwd := root.FirstNonEmpty(root.AsString(ctx.Config()["cwd"]), os.Getenv("OV_TEST_CLAUDE_CWD"))
			if cwd == "" {
				cwd, err = os.MkdirTemp("", "ovtest-claude-"+ctxSafeName(ctx.Name())+"-")
				if err != nil {
					return nil, ctx.ConfigErr("could not create default Claude cwd: " + err.Error())
				}
			}
			cwd, err = filepath.Abs(cwd)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Claude cwd: " + err.Error())
			}
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create Claude cwd: " + err.Error())
			}

			jsonlPath := root.AsString(ctx.Config()["jsonl_path"])
			if jsonlPath == "" {
				jsonlPath = filepath.Join(cwd, ctx.Name()+".jsonl")
			}
			jsonlPath, err = filepath.Abs(jsonlPath)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Claude JSONL path: " + err.Error())
			}
			if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create Claude JSONL directory: " + err.Error())
			}

			env, stateDir, cliConfigPath, debugLogPath, err := claudeOpenVikingEnv(ctx.Name(), ctx.Config(), in["user_key"], cwd)
			if err != nil {
				return nil, err
			}
			pluginRoot, err := claudePluginRoot(ctx.Name(), ctx.Config())
			if err != nil {
				return nil, err
			}

			argv := []string{
				claudeBin(),
				"-p",
				"--plugin-dir", pluginRoot,
				"--output-format", "stream-json",
				"--verbose",
			}
			if root.AsBool(ctx.Config()["isolated_mcp"]) {
				mcpConfigPath, mcpErr := writeClaudeMCPConfig(ctx.Name(), ctx.Config(), filepath.Dir(cliConfigPath), pluginRoot, env)
				if mcpErr != nil {
					return nil, mcpErr
				}
				argv = append(argv, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
			}
			if settings := root.FirstNonEmpty(root.AsString(ctx.Config()["settings"]), os.Getenv("OV_TEST_CLAUDE_SETTINGS")); settings != "" {
				argv = append(argv, "--settings", settings)
			}
			if root.AsBool(ctx.Config()["bypass_permissions"]) {
				argv = append(argv, "--permission-mode", "bypassPermissions")
			}
			if root.AsBool(ctx.Config()["disable_slash_commands"]) {
				argv = append(argv, "--disable-slash-commands")
			}
			if root.AsBool(ctx.Config()["disable_builtin_tools"]) {
				argv = append(argv, "--tools", "")
			}
			if sources, ok := ctx.Config()["setting_sources"]; ok {
				argv = append(argv, "--setting-sources", root.AsString(sources))
			}
			if agents := root.AsString(ctx.Config()["agents"]); agents != "" {
				argv = append(argv, "--agents", agents)
			}
			if tools := root.AsStrings(ctx.Config()["allowed_tools"]); len(tools) > 0 {
				argv = append(argv, "--allowedTools")
				argv = append(argv, tools...)
			}
			if tools := root.AsStrings(ctx.Config()["disallowed_tools"]); len(tools) > 0 {
				argv = append(argv, "--disallowedTools")
				argv = append(argv, tools...)
			}
			if root.AsBool(ctx.Config()["include_hook_events"]) {
				argv = append(argv, "--include-hook-events")
			}
			argv = append(argv, "--debug-file", debugLogPath)
			if model := root.FirstNonEmpty(root.AsString(ctx.Config()["model"]), os.Getenv("OV_TEST_CLAUDE_MODEL")); model != "" {
				argv = append(argv, "--model", model)
			}
			if sid := root.AsString(ctx.Config()["session_id"]); sid != "" {
				argv = append(argv, "--session-id", sid)
			}
			argv = append(argv, message)

			r := ctx.RunCLI(argv, env, 0, root.AsInt(ctx.Config()["timeout"], root.EnvInt("OV_TEST_CLAUDE_TIMEOUT", 600)))
			if err := os.WriteFile(jsonlPath, []byte(r.Stdout), 0o600); err != nil {
				return nil, ctx.ConfigErr("could not write Claude JSONL evidence: " + err.Error())
			}
			if err := ctx.OK(r); err != nil {
				return nil, err
			}
			reply := strings.TrimSpace(extractClaudeReply(r.Stdout))
			if reply == "" {
				return nil, ctx.GateErr("claude exec produced an empty final reply")
			}

			out := root.CLIFields(r)
			out["reply"], out["jsonl"], out["jsonl_path"] = reply, r.Stdout, jsonlPath
			parentID, childID := claudeOVSessionIDs(r.Stdout, root.AsString(ctx.Config()["session_id"]))
			out["ov_session_id"], out["child_ov_session_id"] = parentID, childID
			out["cwd"], out["state_dir"], out["cli_config_path"], out["debug_log_path"] = cwd, stateDir, cliConfigPath, debugLogPath
			return out, nil
		}
	})
}

func claudeOVSessionIDs(jsonl, configured string) (string, string) {
	parent := strings.TrimSpace(configured)
	var task string
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if parent == "" {
			parent = strings.TrimSpace(root.AsString(event["session_id"]))
		}
		if event["type"] == "system" && event["subtype"] == "task_started" {
			task = strings.TrimSpace(root.AsString(event["task_id"]))
		}
	}
	if parent == "" {
		return "", ""
	}
	parentID := "cc-" + parent
	if task == "" {
		return parentID, ""
	}
	return parentID, parentID + "__subagent-" + claudeSafeSessionPart(task)
}

func claudeSafeSessionPart(value string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	return out.String()
}

func claudeMessage(node string, cfg map[string]any, in map[string]any) (string, error) {
	if tmpl := root.AsString(cfg["message_template"]); tmpl != "" {
		return renderTemplate(tmpl, cfg, in), nil
	}
	v, ok := cfg["message"]
	if !ok || v == nil {
		return "", root.ConfigErrorFor(node, "missing required config 'message'")
	}
	return root.AsString(v), nil
}

func renderTemplate(tmpl string, cfg map[string]any, in map[string]any) string {
	out := tmpl
	for key, value := range cfg {
		out = strings.ReplaceAll(out, "{{"+key+"}}", fmt.Sprint(value))
	}
	for key, value := range in {
		out = strings.ReplaceAll(out, "{{"+key+"}}", fmt.Sprint(value))
	}
	return out
}

func claudeOpenVikingEnv(node string, cfg map[string]any, userKey any, cwd string) (map[string]string, string, string, string, error) {
	conf, _ := root.ReadCLIConf(root.UserBaseConf())
	url := root.FirstNonEmpty(
		root.FirstNonEmpty(root.AsString(cfg["openviking_endpoint"]), root.AsString(cfg["openviking_url"])),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CLAUDE_OPENVIKING_ENDPOINT"),
			root.FirstNonEmpty(os.Getenv("OV_TEST_CLAUDE_OPENVIKING_URL"), root.AsString(conf["url"]))),
	)
	if url == "" {
		return nil, "", "", "", root.ConfigErrorFor(node, fmt.Sprintf("could not resolve OpenViking endpoint from %s", root.UserBaseConf()))
	}
	url = strings.TrimRight(url, "/")

	key := root.FirstNonEmpty(root.AsString(cfg["openviking_api_key"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CLAUDE_OPENVIKING_API_KEY"), root.AsString(userKey)))
	if key == "" {
		key = root.AsString(conf["api_key"])
	}
	if key == "" {
		return nil, "", "", "", root.ConfigErrorFor(node, "Claude OpenViking test requires an API key")
	}

	stateDir := root.FirstNonEmpty(root.AsString(cfg["state_dir"]), os.Getenv("OV_TEST_CLAUDE_STATE_DIR"))
	if stateDir == "" {
		stateDir = filepath.Join(cwd, ".openviking-claude-state")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, "", "", "", root.ConfigErrorFor(node, "could not resolve Claude OpenViking state dir: "+err.Error())
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, "", "", "", root.ConfigErrorFor(node, "could not create Claude OpenViking state dir: "+err.Error())
	}

	account := root.FirstNonEmpty(root.AsString(cfg["openviking_account"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CLAUDE_OPENVIKING_ACCOUNT"),
			root.FirstNonEmpty(root.AsString(conf["account"]), root.AsString(conf["account_id"]))))
	user := root.FirstNonEmpty(root.AsString(cfg["openviking_user"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CLAUDE_OPENVIKING_USER"),
			root.FirstNonEmpty(root.AsString(conf["user"]), root.AsString(conf["user_id"]))))
	peerID := root.FirstNonEmpty(root.AsString(cfg["openviking_peer_id"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CLAUDE_OPENVIKING_PEER_ID"),
			root.FirstNonEmpty(root.AsString(conf["actor_peer_id"]),
				root.FirstNonEmpty(root.AsString(conf["peer_id"]), "claude-ovtest"))))

	credentialDir := stateDir
	isolatedHome := ""
	if secretDir := root.SecretStateDir("claude", stateDir); secretDir != "" {
		credentialDir = filepath.Join(secretDir, ctxSafeName(node))
		if err := os.MkdirAll(credentialDir, 0o700); err != nil {
			return nil, "", "", "", root.ConfigErrorFor(node, "could not create Claude secret state dir: "+err.Error())
		}
		isolatedHome = filepath.Join(credentialDir, "home")
		if err := copyClaudeMachineAuth(isolatedHome); err != nil {
			return nil, "", "", "", root.ConfigErrorFor(node, "could not copy Claude auth into isolated home: "+err.Error())
		}
	}
	cliConfigPath := filepath.Join(credentialDir, "ovcli.conf")
	cliConfig := map[string]any{
		"url":           url,
		"api_key":       key,
		"timeout":       120,
		"output":        "json",
		"echo_command":  false,
		"actor_peer_id": peerID,
	}
	if account != "" {
		cliConfig["account"] = account
	}
	if user != "" {
		cliConfig["user"] = user
	}
	body, err := json.MarshalIndent(cliConfig, "", "  ")
	if err != nil {
		return nil, "", "", "", root.ConfigErrorFor(node, "could not encode Claude ovcli config: "+err.Error())
	}
	if err := os.WriteFile(cliConfigPath, append(body, '\n'), 0o600); err != nil {
		return nil, "", "", "", root.ConfigErrorFor(node, "could not write Claude ovcli config: "+err.Error())
	}

	openVikingConfigFile := root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(cfg["openviking_config_file"]), os.Getenv("OV_TEST_OPENVIKING_CONF")), os.Getenv("OPENVIKING_CONFIG_FILE"))
	debugLogPath := root.AsString(cfg["debug_file"])
	if debugLogPath == "" {
		debugLogPath = filepath.Join(stateDir, ctxSafeName(node)+"-debug.log")
	}
	env := map[string]string{
		"OPENVIKING_CREDENTIAL_SOURCE":      "cli",
		"OPENVIKING_CLI_CONFIG_FILE":        cliConfigPath,
		"OPENVIKING_URL":                    url,
		"OPENVIKING_BASE_URL":               url,
		"OPENVIKING_API_KEY":                key,
		"OPENVIKING_AUTH_MODE":              "api_key",
		"OPENVIKING_ACCOUNT":                account,
		"OPENVIKING_USER":                   user,
		"OPENVIKING_PEER_ID":                peerID,
		"OPENVIKING_HOME":                   filepath.Join(stateDir, "home"),
		"OPENVIKING_MEMORY_ENABLED":         "1",
		"OPENVIKING_DEBUG":                  boolEnv(cfg, "debug", "1"),
		"OPENVIKING_DEBUG_LOG":              filepath.Join(stateDir, "claude-hooks.log"),
		"OPENVIKING_AUTO_CAPTURE":           boolEnv(cfg, "auto_capture", "1"),
		"OPENVIKING_AUTO_RECALL":            boolEnv(cfg, "auto_recall", "1"),
		"OPENVIKING_NO_AUTO_INJECT":         boolEnv(cfg, "no_auto_inject", "1"),
		"OPENVIKING_WRITE_PATH_ASYNC":       boolEnv(cfg, "write_path_async", "0"),
		"OPENVIKING_CAPTURE_TIMEOUT_MS":     root.FirstNonEmpty(root.AsString(cfg["capture_timeout_ms"]), "120000"),
		"OPENVIKING_TIMEOUT_MS":             root.FirstNonEmpty(root.AsString(cfg["timeout_ms"]), "120000"),
		"OPENVIKING_SCORE_THRESHOLD":        root.FirstNonEmpty(root.AsString(cfg["score_threshold"]), "0"),
		"OPENVIKING_RECALL_LIMIT":           root.FirstNonEmpty(root.AsString(cfg["recall_limit"]), "8"),
		"OPENVIKING_RECALL_PEER_SCOPE":      root.FirstNonEmpty(root.AsString(cfg["recall_peer_scope"]), "all"),
		"OPENVIKING_COMMIT_TOKEN_THRESHOLD": root.FirstNonEmpty(root.AsString(cfg["commit_token_threshold"]), "1000"),
		"OPENVIKING_PENDING_DIR":            filepath.Join(stateDir, "pending"),
		"OPENVIKING_QUEUE_SCOPE_KEY_FILE":   root.QueueScopeKeyFile("claude", stateDir),
		"NO_COLOR":                          "1",
	}
	if isolatedHome != "" {
		env["HOME"] = isolatedHome
		env["CLAUDE_CONFIG_DIR"] = filepath.Join(isolatedHome, ".claude")
		env["XDG_CONFIG_HOME"] = filepath.Join(isolatedHome, ".config")
		env["XDG_DATA_HOME"] = filepath.Join(isolatedHome, ".local", "share")
		env["XDG_CACHE_HOME"] = filepath.Join(isolatedHome, ".cache")
		env["XDG_STATE_HOME"] = filepath.Join(isolatedHome, ".local", "state")
	}
	if openVikingConfigFile != "" {
		env["OPENVIKING_CONFIG_FILE"] = openVikingConfigFile
	}
	return env, stateDir, cliConfigPath, debugLogPath, nil
}

func writeClaudeMCPConfig(node string, cfg map[string]any, secretDir, pluginRoot string, env map[string]string) (string, error) {
	servers := map[string]any{}
	if !root.AsBool(cfg["disable_mcp"]) {
		proxyPath := filepath.Join(pluginRoot, "servers", "mcp-proxy.mjs")
		if st, err := os.Stat(proxyPath); err != nil || st.IsDir() {
			return "", root.ConfigErrorFor(node, "Claude OpenViking MCP proxy not found at "+proxyPath)
		}
		mcpEnv := map[string]string{}
		for _, key := range []string{
			"HOME", "OPENVIKING_CREDENTIAL_SOURCE", "OPENVIKING_CLI_CONFIG_FILE",
			"OPENVIKING_URL", "OPENVIKING_BASE_URL", "OPENVIKING_API_KEY",
			"OPENVIKING_AUTH_MODE", "OPENVIKING_ACCOUNT", "OPENVIKING_USER",
			"OPENVIKING_PEER_ID", "OPENVIKING_DEBUG", "OPENVIKING_DEBUG_LOG",
			"OPENVIKING_PENDING_DIR", "OPENVIKING_QUEUE_SCOPE_KEY_FILE",
		} {
			if value := env[key]; value != "" {
				mcpEnv[key] = value
			}
		}
		servers["openviking"] = map[string]any{
			"type": "stdio", "command": "node", "args": []string{proxyPath}, "env": mcpEnv,
		}
	}
	body, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not encode Claude MCP config: "+err.Error())
	}
	path := filepath.Join(secretDir, ctxSafeName(node)+"-mcp.json")
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return "", root.ConfigErrorFor(node, "could not write Claude MCP config: "+err.Error())
	}
	return path, nil
}

func copyClaudeMachineAuth(isolatedHome string) error {
	if isolatedHome == "" {
		return nil
	}
	sourceHome := strings.TrimSpace(os.Getenv("OV_TEST_CLAUDE_AUTH_HOME"))
	if sourceHome == "" {
		var err error
		sourceHome, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	for _, relative := range []string{".claude.json", filepath.Join(".claude", ".credentials.json")} {
		source := filepath.Join(sourceHome, relative)
		info, err := os.Lstat(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Claude auth source must be a regular file: %s", source)
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		destination := filepath.Join(isolatedHome, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(destination, raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func claudePluginRoot(node string, cfg map[string]any) (string, error) {
	pluginRoot := root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(cfg["plugin_root"]), root.AsString(cfg["claude_plugin_root"])), os.Getenv("OV_TEST_CLAUDE_PLUGIN_ROOT"))
	if pluginRoot == "" {
		if repo := os.Getenv("OV_TEST_OPENVIKING_REPO"); repo != "" {
			pluginRoot = filepath.Join(repo, "examples", "claude-code-memory-plugin")
		}
	}
	if pluginRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			pluginRoot = filepath.Join(filepath.Dir(wd), "OpenViking", "examples", "claude-code-memory-plugin")
		}
	}
	pluginRoot, err := filepath.Abs(pluginRoot)
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not resolve Claude OpenViking plugin root: "+err.Error())
	}
	pluginRoot, err = filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not canonicalize Claude OpenViking plugin root: "+err.Error())
	}
	manifestPath := filepath.Join(pluginRoot, ".claude-plugin", "plugin.json")
	if st, err := os.Stat(manifestPath); err != nil || st.IsDir() {
		return "", root.ConfigErrorFor(node, "Claude OpenViking plugin manifest not found at "+manifestPath)
	}
	return pluginRoot, nil
}

func boolEnv(cfg map[string]any, key, def string) string {
	if v, ok := cfg[key]; ok {
		switch x := v.(type) {
		case bool:
			if x {
				return "1"
			}
			return "0"
		case string:
			if strings.TrimSpace(x) != "" {
				return strings.TrimSpace(x)
			}
		}
	}
	return def
}

func ctxSafeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "claude"
	}
	return strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(name)
}

func extractClaudeReply(jsonl string) string {
	var last string
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if result := root.AsString(event["result"]); result != "" {
			last = result
			continue
		}
		if event["type"] != "assistant" {
			continue
		}
		if text := assistantText(event["message"]); text != "" {
			last = text
		}
	}
	return last
}

func assistantText(message any) string {
	m, ok := message.(map[string]any)
	if !ok {
		return ""
	}
	content, ok := m["content"].([]any)
	if !ok {
		return ""
	}
	parts := []string{}
	for _, item := range content {
		im, ok := item.(map[string]any)
		if !ok || im["type"] != "text" {
			continue
		}
		if text := root.AsString(im["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func evidenceOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"jsonl", "reply", "after"}, Outputs: []string{"ok", "text", "tools"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			jsonl := root.FirstNonEmpty(root.AsString(in["jsonl"]), root.AsString(ctx.Config()["jsonl"]))
			reply := root.FirstNonEmpty(root.AsString(in["reply"]), root.AsString(ctx.Config()["reply"]))
			text := strings.TrimSpace(successfulMCPResultText(jsonl) + "\n" + reply)
			if strings.TrimSpace(jsonl+"\n"+reply) == "" {
				return nil, ctx.ConfigErr("claude_evidence requires jsonl or reply input")
			}

			missingTools := missingMCPTools(jsonl, tokenList(ctx.Config()["expect_tools"]))
			if len(missingTools) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Claude evidence missing OpenViking MCP tool call(s) %v", missingTools))
			}
			observed := observedOpenVikingTools(jsonl)
			if root.AsBool(ctx.Config()["forbid_any_tool"]) {
				tools := observedClaudeTools(jsonl)
				if len(tools) > 0 {
					names := make([]string, 0, len(tools))
					for name := range tools {
						names = append(names, name)
					}
					sort.Strings(names)
					return nil, ctx.GateErr(fmt.Sprintf("Claude evidence contained forbidden tool call(s) %v", names))
				}
			}
			var forbiddenTools []string
			for _, tool := range tokenList(ctx.Config()["forbid_tools"]) {
				if observed.Contains(canonicalToolName(tool)) {
					forbiddenTools = append(forbiddenTools, tool)
				}
			}
			if len(forbiddenTools) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Claude evidence contained forbidden OpenViking MCP tool call(s) %v", forbiddenTools))
			}
			observedHooks := observedClaudeHooks(jsonl)
			var missingHooks []string
			for _, hook := range tokenList(ctx.Config()["expect_hooks"]) {
				if !observedHooks.Contains(hook) {
					missingHooks = append(missingHooks, hook)
				}
			}
			if len(missingHooks) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Claude evidence missing hook event(s) %v", missingHooks))
			}

			haystack := strings.ToLower(text)
			if missing := root.MissingTokens(haystack, root.LowerAll(tokenList(ctx.Config()["expect"]))); len(missing) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Claude evidence missing expected token(s) %v", missing))
			}
			if leaked := root.PresentTokens(haystack, root.LowerAll(tokenList(ctx.Config()["forbid"]))); len(leaked) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Claude evidence contained forbidden token(s) %v", leaked))
			}
			return map[string]any{
				"ok":    true,
				"text":  text,
				"tools": tokenList(ctx.Config()["expect_tools"]),
			}, nil
		}
	})
}

func observedClaudeHooks(jsonl string) sharedevidence.ToolSet {
	out := sharedevidence.ToolSet{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, key := range []string{"hook_event", "hookEvent", "hook_name", "hookName", "hook_event_name", "hookEventName", "event_name", "eventName"} {
				if value := root.AsString(x[key]); value != "" {
					out.Add(value)
				}
			}
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event any
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			visit(event)
		}
	}
	return out
}

func tokenList(v any) []string {
	if s := strings.TrimSpace(root.AsString(v)); s != "" {
		return []string{s}
	}
	out := []string{}
	for _, item := range root.AsStrings(v) {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func missingMCPTools(jsonl string, tools []string) []string {
	missing := []string{}
	for _, tool := range tools {
		if !mcpToolSeen(jsonl, tool) {
			missing = append(missing, tool)
		}
	}
	return missing
}

func mcpToolSeen(jsonl, tool string) bool {
	tool = strings.ToLower(strings.TrimSpace(tool))
	if tool == "" {
		return true
	}
	return completedMCPTools(jsonl)[tool]
}

func completedMCPTools(jsonl string) map[string]bool {
	calls := map[string]string{}
	completed := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		collectClaudeToolUses(event, calls)
		collectClaudeToolResults(event, calls, completed)
	}
	return completed
}

func collectClaudeToolUses(v any, calls map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		if strings.ToLower(root.AsString(x["type"])) == "tool_use" {
			name := toolNameFromMap(x)
			if isOpenVikingToolName(name) {
				if id := root.AsString(x["id"]); id != "" {
					calls[id] = canonicalToolName(name)
				}
			}
		}
		for _, child := range x {
			collectClaudeToolUses(child, calls)
		}
	case []any:
		for _, child := range x {
			collectClaudeToolUses(child, calls)
		}
	}
}

func collectClaudeToolResults(v any, calls map[string]string, completed map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		if strings.ToLower(root.AsString(x["type"])) == "tool_result" {
			if tool := calls[root.AsString(x["tool_use_id"])]; tool != "" && toolResultSucceeded(x) {
				completed[tool] = true
			}
		}
		for _, child := range x {
			collectClaudeToolResults(child, calls, completed)
		}
	case []any:
		for _, child := range x {
			collectClaudeToolResults(child, calls, completed)
		}
	}
}

func toolResultSucceeded(m map[string]any) bool {
	if v, ok := m["is_error"].(bool); ok && v {
		return false
	}
	content, ok := m["content"]
	return ok && content != nil && !sharedevidence.HasBusinessError(content)
}

func observedClaudeTools(jsonl string) sharedevidence.ToolSet {
	out := sharedevidence.ToolSet{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if strings.EqualFold(root.AsString(x["type"]), "tool_use") {
				if tool := toolNameFromMap(x); tool != "" {
					out.Add(tool)
				}
			}
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event any
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			visit(event)
		}
	}
	return out
}

func observedOpenVikingTools(jsonl string) sharedevidence.ToolSet {
	out := sharedevidence.ToolSet{}
	for tool := range observedClaudeTools(jsonl) {
		if isOpenVikingToolName(tool) {
			out.Add(canonicalToolName(tool))
		}
	}
	return out
}

func successfulMCPResultText(jsonl string) string {
	calls := map[string]string{}
	var parts []string
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		collectClaudeToolUses(event, calls)
		collectClaudeSuccessfulResultText(event, calls, &parts)
	}
	return strings.Join(parts, "\n")
}

func collectClaudeSuccessfulResultText(v any, calls map[string]string, parts *[]string) {
	switch x := v.(type) {
	case map[string]any:
		if strings.EqualFold(root.AsString(x["type"]), "tool_result") && calls[root.AsString(x["tool_use_id"])] != "" && toolResultSucceeded(x) {
			if raw, err := json.Marshal(x["content"]); err == nil {
				*parts = append(*parts, string(raw))
			}
		}
		for _, child := range x {
			collectClaudeSuccessfulResultText(child, calls, parts)
		}
	case []any:
		for _, child := range x {
			collectClaudeSuccessfulResultText(child, calls, parts)
		}
	}
}

func toolNameFromMap(m map[string]any) string {
	for _, key := range []string{"tool", "tool_name", "name", "toolName"} {
		if v := root.AsString(m[key]); v != "" {
			return v
		}
	}
	return ""
}

func isOpenVikingToolName(name string) bool {
	return strings.Contains(strings.ToLower(name), "openviking")
}

func canonicalToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return name
}
