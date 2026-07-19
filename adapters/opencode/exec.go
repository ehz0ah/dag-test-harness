package opencode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// OpenCode adapter: drive the real non-interactive OpenCode CLI with the
// OpenViking OpenCode plugin installed into an isolated OpenCode config dir.
// The harness owns process/env/plugin isolation; capture, recall, and MCP calls
// must happen through OpenCode/plugin behavior.

var (
	Exec     = execOp()
	Evidence = evidenceOp()
)

func opencodeBin() string {
	if v := os.Getenv("OV_TEST_OPENCODE_BIN"); v != "" {
		return v
	}
	return "opencode"
}

func execOp() dag.Factory {
	return root.NewFactory(dag.Meta{
		Inputs:  []string{"user_key", "after", "resource_url", "resource_path", "memory_uri", "case_id"},
		Outputs: []string{"reply", "jsonl", "ov_session_id", "jsonl_path", "project_dir", "state_dir", "cli_config_path", "plugin_config_path", "opencode_config_dir", "debug_log_path"},
	}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			message, err := opencodeMessage(ctx.Name(), ctx.Config(), in)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(message) == "" {
				return nil, ctx.ConfigErr("opencode_exec requires non-empty message")
			}

			projectDir := root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(ctx.Config()["project_dir"]), root.AsString(ctx.Config()["cwd"])), os.Getenv("OV_TEST_OPENCODE_PROJECT_DIR"))
			if projectDir == "" {
				projectDir, err = os.MkdirTemp("", "ovtest-opencode-"+ctxSafeName(ctx.Name())+"-")
				if err != nil {
					return nil, ctx.ConfigErr("could not create default OpenCode project dir: " + err.Error())
				}
			}
			projectDir, err = filepath.Abs(projectDir)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve OpenCode project dir: " + err.Error())
			}
			if err := os.MkdirAll(projectDir, 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create OpenCode project dir: " + err.Error())
			}

			jsonlPath := root.AsString(ctx.Config()["jsonl_path"])
			if jsonlPath == "" {
				jsonlPath = filepath.Join(projectDir, ctx.Name()+".jsonl")
			}
			jsonlPath, err = filepath.Abs(jsonlPath)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve OpenCode JSONL path: " + err.Error())
			}
			if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create OpenCode JSONL directory: " + err.Error())
			}

			env, stateDir, cliConfigPath, pluginConfigPath, opencodeConfigDir, debugLogPath, err := opencodeOpenVikingEnv(ctx.Name(), ctx.Config(), in["user_key"], projectDir)
			if err != nil {
				return nil, err
			}
			pluginRoot, err := opencodePluginRoot(ctx.Name(), ctx.Config())
			if err != nil {
				return nil, err
			}
			if err := installOpenCodePlugin(opencodeConfigDir, pluginRoot); err != nil {
				return nil, ctx.ConfigErr("could not install OpenViking OpenCode plugin: " + err.Error())
			}

			argv := []string{
				opencodeBin(), "run",
				"--auto",
				"--format", "json",
				"--dir", projectDir,
			}
			if model := root.FirstNonEmpty(root.AsString(ctx.Config()["model"]), os.Getenv("OV_TEST_OPENCODE_MODEL")); model != "" {
				argv = append(argv, "--model", model)
			}
			argv = append(argv, message)

			emptyReplyRetries := root.AsInt(ctx.Config()["empty_reply_retries"], 0)
			if emptyReplyRetries < 0 || emptyReplyRetries > 2 {
				return nil, ctx.ConfigErr("empty_reply_retries must be between 0 and 2")
			}
			var r root.CLIResult
			var reply string
			attempts := make([]string, 0, emptyReplyRetries+1)
			for attempt := 0; attempt <= emptyReplyRetries; attempt++ {
				r = ctx.RunCLI(argv, env, 0, root.AsInt(ctx.Config()["timeout"], root.EnvInt("OV_TEST_OPENCODE_TIMEOUT", 900)))
				attempts = append(attempts, r.Stdout)
				if r.ExitCode != 0 {
					break
				}
				reply = strings.TrimSpace(extractOpenCodeReply(r.Stdout))
				if reply != "" {
					break
				}
			}
			jsonl := strings.Join(attempts, "\n")
			if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
				return nil, ctx.ConfigErr("could not write OpenCode JSONL evidence: " + err.Error())
			}
			if err := ctx.OK(r); err != nil {
				return nil, err
			}
			if reply == "" && !root.AsBool(ctx.Config()["allow_empty_reply"]) {
				return nil, ctx.GateErr("opencode run produced an empty final reply")
			}

			out := root.CLIFields(r)
			out["reply"], out["jsonl"], out["jsonl_path"] = reply, jsonl, jsonlPath
			out["ov_session_id"] = openCodeOVSessionID(jsonl)
			out["project_dir"], out["state_dir"], out["cli_config_path"] = projectDir, stateDir, cliConfigPath
			out["plugin_config_path"], out["opencode_config_dir"], out["debug_log_path"] = pluginConfigPath, opencodeConfigDir, debugLogPath
			return out, nil
		}
	})
}

func openCodeOVSessionID(jsonl string) string {
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		id := strings.TrimSpace(root.AsString(event["sessionID"]))
		if id == "" {
			if part, _ := event["part"].(map[string]any); part != nil {
				id = strings.TrimSpace(root.AsString(part["sessionID"]))
			}
		}
		if id != "" {
			return "oc-" + id
		}
	}
	return ""
}

func opencodeMessage(node string, cfg map[string]any, in map[string]any) (string, error) {
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

func opencodeOpenVikingEnv(node string, cfg map[string]any, userKey any, projectDir string) (map[string]string, string, string, string, string, string, error) {
	conf, _ := root.ReadCLIConf(root.UserBaseConf())
	url := root.FirstNonEmpty(
		root.FirstNonEmpty(root.AsString(cfg["openviking_endpoint"]), root.AsString(cfg["openviking_url"])),
		root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCODE_OPENVIKING_ENDPOINT"),
			root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCODE_OPENVIKING_URL"), root.AsString(conf["url"]))),
	)
	if url == "" {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, fmt.Sprintf("could not resolve OpenViking endpoint from %s", root.UserBaseConf()))
	}
	url = strings.TrimRight(url, "/")

	key := root.FirstNonEmpty(root.AsString(cfg["openviking_api_key"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCODE_OPENVIKING_API_KEY"), root.AsString(userKey)))
	if key == "" {
		key = root.AsString(conf["api_key"])
	}
	if key == "" {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "OpenCode OpenViking test requires an API key")
	}

	stateDir := root.FirstNonEmpty(root.AsString(cfg["state_dir"]), os.Getenv("OV_TEST_OPENCODE_STATE_DIR"))
	if stateDir == "" {
		stateDir = filepath.Join(projectDir, ".openviking-opencode-state")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not resolve OpenCode state dir: "+err.Error())
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not create OpenCode state dir: "+err.Error())
	}

	account := root.FirstNonEmpty(root.AsString(cfg["openviking_account"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCODE_OPENVIKING_ACCOUNT"),
			root.FirstNonEmpty(root.AsString(conf["account"]), root.AsString(conf["account_id"]))))
	user := root.FirstNonEmpty(root.AsString(cfg["openviking_user"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCODE_OPENVIKING_USER"),
			root.FirstNonEmpty(root.AsString(conf["user"]), root.AsString(conf["user_id"]))))
	peerID := root.FirstNonEmpty(root.AsString(cfg["openviking_peer_id"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_OPENCODE_OPENVIKING_PEER_ID"),
			root.FirstNonEmpty(root.AsString(conf["actor_peer_id"]),
				root.FirstNonEmpty(root.AsString(conf["peer_id"]), "opencode-ovtest"))))

	credentialDir := stateDir
	if secretDir := root.SecretStateDir("opencode", stateDir); secretDir != "" {
		credentialDir = secretDir
		if err := os.MkdirAll(credentialDir, 0o700); err != nil {
			return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not create OpenCode secret state dir: "+err.Error())
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
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not encode OpenCode ovcli config: "+err.Error())
	}
	if err := os.WriteFile(cliConfigPath, append(body, '\n'), 0o600); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not write OpenCode ovcli config: "+err.Error())
	}

	opencodeConfigDir := filepath.Join(credentialDir, "opencode-config")
	if err := os.MkdirAll(opencodeConfigDir, 0o700); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not create OpenCode config dir: "+err.Error())
	}
	if err := copyOpenCodeConfigTemplate(opencodeConfigDir, cfg); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not copy OpenCode config template: "+err.Error())
	}
	openVikingRuntimeDir := filepath.Join(stateDir, "openviking-runtime")
	if err := os.MkdirAll(openVikingRuntimeDir, 0o700); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not create OpenViking plugin runtime dir: "+err.Error())
	}
	debugLogPath := filepath.Join(openVikingRuntimeDir, "opencode-openviking.log")
	pluginConfigPath := filepath.Join(stateDir, "openviking-config.json")
	if err := writePluginConfig(pluginConfigPath, openVikingRuntimeDir, debugLogPath, cfg); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not write OpenViking OpenCode plugin config: "+err.Error())
	}

	openVikingConfigFile := root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(cfg["openviking_config_file"]), os.Getenv("OV_TEST_OPENVIKING_CONF")), os.Getenv("OPENVIKING_CONFIG_FILE"))
	if openVikingConfigFile == "" {
		openVikingConfigFile = filepath.Join(stateDir, "ov.conf")
		if err := writeMinimalOpenVikingConfig(openVikingConfigFile, url); err != nil {
			return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not write OpenCode OpenViking server config: "+err.Error())
		}
	}
	dataHome := filepath.Join(credentialDir, "xdg-data")
	configHome := filepath.Join(credentialDir, "xdg-config")
	home := filepath.Join(stateDir, "home")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not create OpenCode isolated XDG config home: "+err.Error())
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not create OpenCode isolated home: "+err.Error())
	}
	env := map[string]string{
		"HOME":                              home,
		"OPENCODE_TEST_HOME":                home,
		"OPENCODE_CONFIG_DIR":               opencodeConfigDir,
		"XDG_CONFIG_HOME":                   configHome,
		"XDG_DATA_HOME":                     dataHome,
		"XDG_CACHE_HOME":                    filepath.Join(stateDir, "xdg-cache"),
		"XDG_STATE_HOME":                    filepath.Join(stateDir, "xdg-state"),
		"OPENVIKING_CREDENTIAL_SOURCE":      "cli",
		"OPENVIKING_CLI_CONFIG_FILE":        cliConfigPath,
		"OPENVIKING_PLUGIN_CONFIG":          pluginConfigPath,
		"OPENVIKING_URL":                    url,
		"OPENVIKING_BASE_URL":               url,
		"OPENVIKING_API_KEY":                key,
		"OPENVIKING_AUTH_MODE":              "api_key",
		"OPENVIKING_ACCOUNT":                account,
		"OPENVIKING_USER":                   user,
		"OPENVIKING_PEER_ID":                peerID,
		"OPENVIKING_DEBUG":                  boolEnv(cfg, "debug", "1"),
		"OPENVIKING_DEBUG_LOG":              debugLogPath,
		"OPENVIKING_AUTO_CAPTURE":           boolEnv(cfg, "auto_capture", "1"),
		"OPENVIKING_AUTO_RECALL":            boolEnv(cfg, "auto_recall", "1"),
		"OPENVIKING_NO_AUTO_INJECT":         boolEnv(cfg, "no_auto_inject", "0"),
		"OPENVIKING_CAPTURE_TIMEOUT_MS":     root.FirstNonEmpty(root.AsString(cfg["capture_timeout_ms"]), "120000"),
		"OPENVIKING_TIMEOUT_MS":             root.FirstNonEmpty(root.AsString(cfg["timeout_ms"]), "120000"),
		"OPENVIKING_SCORE_THRESHOLD":        root.FirstNonEmpty(root.AsString(cfg["score_threshold"]), "0"),
		"OPENVIKING_RECALL_LIMIT":           root.FirstNonEmpty(root.AsString(cfg["recall_limit"]), "8"),
		"OPENVIKING_COMMIT_TOKEN_THRESHOLD": root.FirstNonEmpty(root.AsString(cfg["commit_token_threshold"]), "1000"),
		"OPENVIKING_WORKSPACE_PEER":         boolEnv(cfg, "workspace_peer", "0"),
		"OPENVIKING_PENDING_DIR":            filepath.Join(stateDir, "pending"),
		"OPENVIKING_QUEUE_SCOPE_KEY_FILE":   root.QueueScopeKeyFile("opencode", stateDir),
		"OPENVIKING_CONFIG_FILE":            openVikingConfigFile,
		"NO_COLOR":                          "1",
	}
	if err := copyOpenCodeAuth(env["XDG_DATA_HOME"]); err != nil {
		return nil, "", "", "", "", "", root.ConfigErrorFor(node, "could not copy OpenCode auth into isolated data dir: "+err.Error())
	}
	return env, stateDir, cliConfigPath, pluginConfigPath, opencodeConfigDir, debugLogPath, nil
}

func copyOpenCodeAuth(isolatedDataHome string) error {
	explicit := strings.TrimSpace(os.Getenv("OV_TEST_OPENCODE_AUTH_FILE"))
	source := explicit
	if source == "" {
		dataHome := strings.TrimSpace(os.Getenv("OV_TEST_OPENCODE_AUTH_DATA_HOME"))
		if dataHome == "" {
			dataHome = strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
		}
		if dataHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			dataHome = filepath.Join(home, ".local", "share")
		}
		source = filepath.Join(dataHome, "opencode", "auth.json")
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		if os.IsNotExist(err) && explicit == "" {
			return nil
		}
		return err
	}
	destinationDir := filepath.Join(isolatedDataHome, "opencode")
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destinationDir, "auth.json"), raw, 0o600)
}

func writeMinimalOpenVikingConfig(path, url string) error {
	body := map[string]any{
		"url": url,
		"server": map[string]any{
			"url": url,
		},
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func copyOpenCodeConfigTemplate(configDir string, cfg map[string]any) error {
	template := root.FirstNonEmpty(root.AsString(cfg["opencode_config_template"]), os.Getenv("OV_TEST_OPENCODE_CONFIG_TEMPLATE"))
	if strings.TrimSpace(template) == "" {
		return nil
	}
	template, err := filepath.Abs(template)
	if err != nil {
		return err
	}
	info, err := os.Stat(template)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("template is a directory: %s", template)
	}
	raw, err := os.ReadFile(template)
	if err != nil {
		return err
	}
	var source map[string]any
	if err := json.Unmarshal(stripJSONC(raw), &source); err != nil {
		return fmt.Errorf("decode OpenCode provider template: %w", err)
	}
	clean := map[string]any{}
	for _, key := range []string{"$schema", "provider", "model", "small_model", "enabled_providers", "disabled_providers"} {
		if value, ok := source[key]; ok {
			clean[key] = value
		}
	}
	out, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "opencode.json"), append(out, '\n'), 0o600)
}

// stripJSONC removes comments and trailing commas while preserving string
// contents. OpenCode accepts JSONC, but ovtest writes a minimal strict-JSON
// provider/model projection into the isolated configuration directory.
func stripJSONC(raw []byte) []byte {
	withoutComments := make([]byte, 0, len(raw))
	inString, escaped, lineComment, blockComment := false, false, false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if lineComment {
			if c == '\n' {
				lineComment = false
				withoutComments = append(withoutComments, c)
			}
			continue
		}
		if blockComment {
			if c == '*' && i+1 < len(raw) && raw[i+1] == '/' {
				blockComment = false
				i++
			} else if c == '\n' {
				withoutComments = append(withoutComments, c)
			}
			continue
		}
		if inString {
			withoutComments = append(withoutComments, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			withoutComments = append(withoutComments, c)
			continue
		}
		if c == '/' && i+1 < len(raw) {
			switch raw[i+1] {
			case '/':
				lineComment = true
				i++
				continue
			case '*':
				blockComment = true
				i++
				continue
			}
		}
		withoutComments = append(withoutComments, c)
	}

	out := make([]byte, 0, len(withoutComments))
	inString, escaped = false, false
	for i, c := range withoutComments {
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(withoutComments) && (withoutComments[j] == ' ' || withoutComments[j] == '\t' || withoutComments[j] == '\r' || withoutComments[j] == '\n') {
				j++
			}
			if j < len(withoutComments) && (withoutComments[j] == '}' || withoutComments[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func writePluginConfig(path, runtimeDir, debugLogPath string, cfg map[string]any) error {
	body := map[string]any{
		"enabled":               true,
		"runtime":               map[string]any{"dataDir": runtimeDir},
		"repoContext":           map[string]any{"enabled": false},
		"autoCapture":           configBool(cfg, "auto_capture", true),
		"captureMode":           root.FirstNonEmpty(root.AsString(cfg["capture_mode"]), "semantic"),
		"captureAssistantTurns": configBool(cfg, "capture_assistant_turns", true),
		"autoRecall": map[string]any{
			"enabled":         configBool(cfg, "auto_recall", true),
			"limit":           root.AsInt(cfg["recall_limit"], 8),
			"scoreThreshold":  configFloatString(cfg, "score_threshold", 0),
			"maxContentChars": root.AsInt(cfg["recall_max_content_chars"], 1200),
			"preferAbstract":  configBool(cfg, "recall_prefer_abstract", true),
			"tokenBudget":     root.AsInt(cfg["recall_token_budget"], 4000),
			"minQueryLength":  root.AsInt(cfg["min_query_length"], 3),
		},
		"commitTokenThreshold":  root.AsInt(cfg["commit_token_threshold"], 1000),
		"commitKeepRecentCount": root.AsInt(cfg["commit_keep_recent_count"], 10),
		"debug":                 configBool(cfg, "debug", true),
		"debugLogPath":          debugLogPath,
		"workspacePeer":         configBool(cfg, "workspace_peer", false),
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func configBool(cfg map[string]any, key string, def bool) bool {
	if v, ok := cfg[key]; ok {
		switch x := v.(type) {
		case bool:
			return x
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "1", "true", "yes", "on":
				return true
			case "0", "false", "no", "off":
				return false
			}
		}
	}
	return def
}

func configFloatString(cfg map[string]any, key string, def float64) any {
	if v, ok := cfg[key]; ok {
		if s := strings.TrimSpace(root.AsString(v)); s != "" {
			return s
		}
		return v
	}
	return def
}

func installOpenCodePlugin(configDir, pluginRoot string) error {
	wrapperSrc := filepath.Join(pluginRoot, "wrappers", "openviking.js")
	if st, err := os.Stat(wrapperSrc); err != nil || st.IsDir() {
		return fmt.Errorf("OpenViking OpenCode wrapper not found at %s", wrapperSrc)
	}
	pkgSrc := pluginRoot
	for _, required := range []string{"index.mjs", "package.json", "lib", "servers"} {
		if _, err := os.Stat(filepath.Join(pkgSrc, required)); err != nil {
			return fmt.Errorf("OpenViking OpenCode plugin missing %s under %s", required, pkgSrc)
		}
	}
	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		return err
	}
	if err := copyFile(wrapperSrc, filepath.Join(pluginsDir, "openviking.js")); err != nil {
		return err
	}
	dst := filepath.Join(pluginsDir, "openviking")
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"index.mjs", "package.json", "lib", "servers"} {
		if err := copyPath(filepath.Join(pkgSrc, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func opencodePluginRoot(node string, cfg map[string]any) (string, error) {
	pluginRoot := root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(cfg["plugin_root"]), root.AsString(cfg["opencode_plugin_root"])), os.Getenv("OV_TEST_OPENCODE_PLUGIN_ROOT"))
	if pluginRoot == "" {
		if repo := os.Getenv("OV_TEST_OPENVIKING_REPO"); repo != "" {
			pluginRoot = filepath.Join(repo, "examples", "opencode-plugin")
		}
	}
	if pluginRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			pluginRoot = filepath.Join(filepath.Dir(wd), "OpenViking", "examples", "opencode-plugin")
		}
	}
	pluginRoot, err := filepath.Abs(pluginRoot)
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not resolve OpenViking OpenCode plugin root: "+err.Error())
	}
	pluginRoot, err = filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not canonicalize OpenViking OpenCode plugin root: "+err.Error())
	}
	if st, err := os.Stat(filepath.Join(pluginRoot, "index.mjs")); err != nil || st.IsDir() {
		return "", root.ConfigErrorFor(node, "OpenViking OpenCode plugin index not found at "+filepath.Join(pluginRoot, "index.mjs"))
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
		return "opencode"
	}
	return strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(name)
}

func extractOpenCodeReply(jsonl string) string {
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
		if text := opencodeText(event); text != "" {
			last = text
		}
	}
	return last
}

func opencodeText(event map[string]any) string {
	if strings.ToLower(root.AsString(event["type"])) == "text" {
		if text := root.AsString(event["text"]); text != "" {
			return text
		}
	}
	part, _ := event["part"].(map[string]any)
	if part == nil {
		return ""
	}
	if strings.ToLower(root.AsString(part["type"])) != "text" {
		return ""
	}
	return root.AsString(part["text"])
}

func evidenceOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"jsonl", "reply", "after"}, Outputs: []string{"ok", "text", "tools"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			jsonl := root.FirstNonEmpty(root.AsString(in["jsonl"]), root.AsString(ctx.Config()["jsonl"]))
			reply := root.FirstNonEmpty(root.AsString(in["reply"]), root.AsString(ctx.Config()["reply"]))
			text := strings.TrimSpace(successfulMCPResultText(jsonl) + "\n" + reply)
			if strings.TrimSpace(jsonl+"\n"+reply) == "" {
				return nil, ctx.ConfigErr("opencode_evidence requires jsonl or reply input")
			}

			missingTools := missingMCPTools(jsonl, tokenList(ctx.Config()["expect_tools"]))
			if len(missingTools) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("OpenCode evidence missing OpenViking MCP tool call(s) %v", missingTools))
			}
			observed := observedOpenVikingTools(jsonl)
			var forbiddenTools []string
			for _, tool := range tokenList(ctx.Config()["forbid_tools"]) {
				if observed.Contains(canonicalToolName(tool)) {
					forbiddenTools = append(forbiddenTools, tool)
				}
			}
			if len(forbiddenTools) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("OpenCode evidence contained forbidden OpenViking MCP tool call(s) %v", forbiddenTools))
			}

			haystack := strings.ToLower(text)
			if missing := root.MissingTokens(haystack, root.LowerAll(tokenList(ctx.Config()["expect"]))); len(missing) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("OpenCode evidence missing expected token(s) %v", missing))
			}
			if leaked := root.PresentTokens(haystack, root.LowerAll(tokenList(ctx.Config()["forbid"]))); len(leaked) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("OpenCode evidence contained forbidden token(s) %v", leaked))
			}
			return map[string]any{
				"ok":    true,
				"text":  text,
				"tools": tokenList(ctx.Config()["expect_tools"]),
			}, nil
		}
	})
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
	out := map[string]bool{}
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
		collectCompletedMCPTools(event, out)
	}
	return out
}

func collectCompletedMCPTools(v any, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		if isCompletedOpenVikingToolEvent(x) {
			part, _ := x["part"].(map[string]any)
			out[canonicalToolName(toolNameFromMap(part))] = true
		}
		for _, child := range x {
			collectCompletedMCPTools(child, out)
		}
	case []any:
		for _, child := range x {
			collectCompletedMCPTools(child, out)
		}
	}
}

func isCompletedOpenVikingToolEvent(event map[string]any) bool {
	if strings.ToLower(root.AsString(event["type"])) != "tool_use" {
		return false
	}
	part, _ := event["part"].(map[string]any)
	if part == nil || strings.ToLower(root.AsString(part["type"])) != "tool" {
		return false
	}
	rawTool := toolNameFromMap(part)
	if rawTool == "" || !strings.Contains(strings.ToLower(rawTool), "openviking") {
		return false
	}
	state, _ := part["state"].(map[string]any)
	if state == nil || strings.ToLower(root.AsString(state["status"])) != "completed" {
		return false
	}
	if errValue, ok := state["error"]; ok && errValue != nil && strings.TrimSpace(fmt.Sprint(errValue)) != "" {
		return false
	}
	return !sharedevidence.HasBusinessError(state["output"])
}

func observedOpenVikingTools(jsonl string) sharedevidence.ToolSet {
	out := sharedevidence.ToolSet{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if strings.EqualFold(root.AsString(x["type"]), "tool_use") {
				part, _ := x["part"].(map[string]any)
				if tool := toolNameFromMap(part); strings.Contains(strings.ToLower(tool), "openviking") {
					out.Add(canonicalToolName(tool))
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

func successfulMCPResultText(jsonl string) string {
	var parts []string
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if isCompletedOpenVikingToolEvent(x) {
				part, _ := x["part"].(map[string]any)
				state, _ := part["state"].(map[string]any)
				if raw, err := json.Marshal(state["output"]); err == nil {
					parts = append(parts, string(raw))
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
	return strings.Join(parts, "\n")
}

func toolNameFromMap(m map[string]any) string {
	for _, key := range []string{"tool", "tool_name", "name", "toolName"} {
		if v := root.AsString(m[key]); v != "" {
			return v
		}
	}
	return ""
}

func canonicalToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	name = strings.TrimPrefix(name, "mcp__")
	name = strings.TrimPrefix(name, "openviking_")
	name = strings.TrimPrefix(name, "openviking-")
	return name
}
