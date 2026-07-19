package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// Codex adapter: drive the real non-interactive Codex CLI with the installed
// OpenViking plugin. The harness owns only process/env isolation; memory writes,
// recalls, and MCP calls must happen through Codex/plugin behavior.

var (
	Exec               = execOp()
	Evidence           = evidenceOp()
	installCodexPlugin = installCodexPluginCommand
	trustCodexHooks    = trustCodexPluginHooks
)

func codexBin() string {
	if v := os.Getenv("OV_TEST_CODEX_BIN"); v != "" {
		return v
	}
	return "codex"
}

func execOp() dag.Factory {
	return root.NewFactory(dag.Meta{
		Inputs:  []string{"user_key", "after", "resource_url", "resource_path", "memory_uri", "case_id"},
		Outputs: []string{"reply", "jsonl", "ov_session_id", "output_path", "jsonl_path", "cwd", "state_dir", "cli_config_path", "codex_home", "codex_config_path"},
	}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			message, err := codexMessage(ctx.Name(), ctx.Config(), in)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(message) == "" {
				return nil, ctx.ConfigErr("codex_exec requires non-empty message")
			}

			cwd := root.FirstNonEmpty(root.AsString(ctx.Config()["cwd"]), os.Getenv("OV_TEST_CODEX_CWD"))
			if cwd == "" {
				cwd, err = os.MkdirTemp("", "ovtest-codex-"+ctxSafeName(ctx.Name())+"-")
				if err != nil {
					return nil, ctx.ConfigErr("could not create default Codex cwd: " + err.Error())
				}
			}
			cwd, err = filepath.Abs(cwd)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Codex cwd: " + err.Error())
			}
			if err := os.MkdirAll(cwd, 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create Codex cwd: " + err.Error())
			}

			outputPath := root.AsString(ctx.Config()["output_path"])
			if outputPath == "" {
				outputPath = filepath.Join(cwd, ctx.Name()+".last-message.txt")
			}
			outputPath, err = filepath.Abs(outputPath)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Codex output path: " + err.Error())
			}
			if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create Codex output directory: " + err.Error())
			}
			jsonlPath := root.AsString(ctx.Config()["jsonl_path"])
			if jsonlPath == "" {
				jsonlPath = filepath.Join(cwd, ctx.Name()+".jsonl")
			}
			jsonlPath, err = filepath.Abs(jsonlPath)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Codex JSONL path: " + err.Error())
			}
			if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create Codex JSONL directory: " + err.Error())
			}

			envExtra, shellEnv, stateDir, cliConfigPath, err := codexOpenVikingEnv(ctx.Name(), ctx.Config(), in["user_key"], cwd)
			if err != nil {
				return nil, err
			}
			codexHome, codexConfigPath, err := prepareCodexHome(ctx.Name(), ctx.Config(), stateDir)
			if err != nil {
				return nil, err
			}
			envExtra["CODEX_HOME"] = codexHome
			if root.AsBool(ctx.Config()["isolated_mcp"]) {
				if err := writeIsolatedMCPConfig(ctx.Name(), ctx.Config(), codexConfigPath, shellEnv); err != nil {
					return nil, err
				}
			} else {
				if err := installCodexPlugin(ctx, codexBin(), codexHome); err != nil {
					return nil, err
				}
				if err := trustCodexHooks(ctx.Context(), codexBin(), codexHome, cwd); err != nil {
					return nil, ctx.ConfigErr("could not trust isolated OpenViking Codex hooks: " + err.Error())
				}
			}

			argv := []string{
				codexBin(), "exec",
				"-c", "shell_environment_policy.set=" + tomlInlineStringMap(shellEnv),
				"--json",
				"--skip-git-repo-check",
				"-C", cwd,
				"--output-last-message", outputPath,
			}
			if root.AsBool(ctx.Config()["isolated_mcp"]) {
				// The isolated CODEX_HOME contains only the explicit MCP server.
				// Loading that file is more robust than command-line dotted overrides
				// and keeps server configuration out of process arguments.
				argv = append(argv, "--strict-config")
			}
			if root.AsBool(ctx.Config()["bypass_approvals"]) {
				argv = append(argv, "--dangerously-bypass-approvals-and-sandbox")
			} else if sandbox := root.FirstNonEmpty(root.AsString(ctx.Config()["sandbox"]), "workspace-write"); sandbox != "" {
				argv = append(argv, "--sandbox", sandbox)
			}
			if model := root.FirstNonEmpty(root.AsString(ctx.Config()["model"]), os.Getenv("OV_TEST_CODEX_MODEL")); model != "" {
				argv = append(argv, "--model", model)
			}
			argv = append(argv, message)

			r := ctx.RunCLI(argv, envExtra, 0, root.AsInt(ctx.Config()["timeout"], root.EnvInt("OV_TEST_CODEX_TIMEOUT", 600)))
			if err := os.WriteFile(jsonlPath, []byte(r.Stdout), 0o600); err != nil {
				return nil, ctx.ConfigErr("could not write Codex JSONL evidence: " + err.Error())
			}
			if message := codexUsageLimitError(r.Stdout); message != "" {
				return nil, fmt.Errorf("%s: Codex environment unavailable: %s", ctx.Name(), message)
			}
			if err := ctx.OK(r); err != nil {
				return nil, err
			}
			replyRaw, readErr := os.ReadFile(outputPath)
			reply := strings.TrimSpace(string(replyRaw))
			if readErr != nil || reply == "" {
				reply = strings.TrimSpace(extractLastMessageFromJSONL(r.Stdout))
			}
			if reply == "" {
				return nil, ctx.GateErr("codex exec produced an empty final reply")
			}

			out := root.CLIFields(r)
			out["reply"], out["jsonl"], out["output_path"], out["jsonl_path"], out["cwd"], out["state_dir"], out["cli_config_path"] = reply, r.Stdout, outputPath, jsonlPath, cwd, stateDir, cliConfigPath
			out["codex_home"], out["codex_config_path"] = codexHome, codexConfigPath
			out["ov_session_id"] = codexOVSessionID(r.Stdout)
			return out, nil
		}
	})
}

func codexUsageLimitError(jsonl string) string {
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || (event.Type != "error" && event.Type != "turn.failed") {
			continue
		}
		message := strings.TrimSpace(root.FirstNonEmpty(event.Message, event.Error.Message))
		lower := strings.ToLower(message)
		if strings.Contains(lower, "usage limit") && strings.Contains(lower, "purchase more credits") {
			return message
		}
	}
	return ""
}

func codexOVSessionID(jsonl string) string {
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event["type"] != "thread.started" {
			continue
		}
		id := strings.TrimSpace(root.AsString(event["thread_id"]))
		if id != "" {
			return "cx-" + sanitizeSessionID(id, "_")
		}
	}
	return ""
}

func sanitizeSessionID(value, replacement string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteString(replacement)
		}
	}
	return out.String()
}

type codexHookMetadata struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	Source      string `json:"source"`
	PluginID    string `json:"pluginId"`
	Enabled     bool   `json:"enabled"`
	CurrentHash string `json:"currentHash"`
}

type codexHooksListResponse struct {
	Data []struct {
		Hooks    []codexHookMetadata `json:"hooks"`
		Warnings []string            `json:"warnings"`
		Errors   []any               `json:"errors"`
	} `json:"data"`
}

// trustCodexPluginHooks asks Codex for its own hook identities and hashes, then
// persists trust only inside the disposable CODEX_HOME. This avoids coupling
// ovtest to Codex's private hash algorithm and remains reliable when the
// one-shot hook-trust bypass is ineffective, without touching operator config.
func trustCodexPluginHooks(parent context.Context, binary, codexHome, cwd string) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "app-server", "--strict-config")
	cmd.Env = codexProcessEnv(map[string]string{"CODEX_HOME": codexHome})
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	finished := false
	defer func() {
		_ = stdin.Close()
		if !finished && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	enc := json.NewEncoder(stdin)
	for _, request := range []any{
		map[string]any{
			"method": "initialize", "id": 0,
			"params": map[string]any{
				"clientInfo":   map[string]any{"name": "ovtest", "title": "ovtest", "version": "1"},
				"capabilities": map[string]any{"experimentalApi": true},
			},
		},
		map[string]any{"method": "initialized", "params": map[string]any{}},
		map[string]any{"method": "hooks/list", "id": 1, "params": map[string]any{"cwds": []string{cwd}}},
	} {
		if err := enc.Encode(request); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	listRaw, err := codexRPCResult(scanner, 1)
	if err != nil {
		return fmt.Errorf("hooks/list: %w", err)
	}
	var listed codexHooksListResponse
	if err := json.Unmarshal(listRaw, &listed); err != nil {
		return fmt.Errorf("decode hooks/list result: %w", err)
	}
	if len(listed.Data) != 1 {
		return fmt.Errorf("hooks/list returned %d cwd results, want 1", len(listed.Data))
	}
	if len(listed.Data[0].Errors) > 0 {
		return fmt.Errorf("hooks/list reported errors: %v", listed.Data[0].Errors)
	}

	const pluginID = "openviking-memory@openviking"
	required := map[string]bool{
		"sessionStart": false, "userPromptSubmit": false, "stop": false, "preCompact": false,
	}
	states := map[string]any{}
	for _, hook := range listed.Data[0].Hooks {
		if hook.Source != "plugin" || hook.PluginID != pluginID {
			continue
		}
		if !hook.Enabled || strings.TrimSpace(hook.Key) == "" || strings.TrimSpace(hook.CurrentHash) == "" {
			return fmt.Errorf("OpenViking hook %q is disabled or missing identity/hash", hook.EventName)
		}
		if _, ok := required[hook.EventName]; ok {
			required[hook.EventName] = true
		}
		states[hook.Key] = map[string]any{"enabled": true, "trusted_hash": hook.CurrentHash}
	}
	var missing []string
	for event, found := range required {
		if !found {
			missing = append(missing, event)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("OpenViking plugin is missing required hook event(s): %s", strings.Join(missing, ", "))
	}

	writeRequest := map[string]any{
		"method": "config/batchWrite", "id": 2,
		"params": map[string]any{
			"edits": []any{map[string]any{
				"keyPath": "hooks.state", "value": states, "mergeStrategy": "upsert",
			}},
			"reloadUserConfig": true,
		},
	}
	if err := enc.Encode(writeRequest); err != nil {
		return err
	}
	if _, err := codexRPCResult(scanner, 2); err != nil {
		return fmt.Errorf("config/batchWrite: %w", err)
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	waitErr := cmd.Wait()
	finished = true
	if waitErr != nil {
		return fmt.Errorf("Codex app-server exited after hook trust write: %w%s", waitErr, codexStderrDetail(stderr.String()))
	}
	return nil
}

func codexRPCResult(scanner *bufio.Scanner, expectedID int) (json.RawMessage, error) {
	for scanner.Scan() {
		var envelope struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &envelope) != nil || envelope.ID == nil || *envelope.ID != expectedID {
			continue
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return nil, fmt.Errorf("RPC error: %s", envelope.Error)
		}
		if len(envelope.Result) == 0 {
			return nil, fmt.Errorf("RPC response has no result")
		}
		return envelope.Result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("Codex app-server closed before response id %d", expectedID)
}

func codexProcessEnv(extra map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(extra))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replace := extra[key]; !replace {
			out = append(out, item)
		}
	}
	for key, value := range extra {
		out = append(out, key+"="+value)
	}
	return out
}

func codexStderrDetail(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return ": " + root.Truncate(stderr, 1000)
}

func installCodexPluginCommand(ctx *root.OpContext, binary, codexHome string) error {
	r := ctx.RunCLI([]string{binary, "plugin", "add", "openviking-memory@openviking"}, map[string]string{
		"CODEX_HOME": codexHome,
	}, 0, 60)
	if r.ExitCode != 0 {
		return ctx.ConfigErr("could not install OpenViking plugin into isolated Codex home: " + root.ExitDetail(r))
	}
	return nil
}

func prepareCodexHome(node string, cfg map[string]any, stateDir string) (string, string, error) {
	codexHome := filepath.Join(stateDir, "codex-home")
	if secretDir := root.SecretStateDir("codex", stateDir); secretDir != "" {
		codexHome = filepath.Join(secretDir, ctxSafeName(node), "codex-home")
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return "", "", root.ConfigErrorFor(node, "could not create isolated CODEX_HOME: "+err.Error())
	}
	pluginRoot, err := codexPluginRoot(node, cfg)
	if err != nil {
		return "", "", err
	}
	marketplaceRoot := filepath.Dir(pluginRoot)
	configPath := filepath.Join(codexHome, "config.toml")
	config := fmt.Sprintf(`[plugins."openviking-memory@openviking"]
enabled = true

[marketplaces.openviking]
source_type = "local"
source = %s

[features]
hooks = true
`, tomlQuote(marketplaceRoot))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return "", "", root.ConfigErrorFor(node, "could not write isolated Codex config: "+err.Error())
	}

	authSource := strings.TrimSpace(os.Getenv("OV_TEST_CODEX_AUTH_FILE"))
	if authSource == "" {
		authHome := strings.TrimSpace(os.Getenv("OV_TEST_CODEX_AUTH_HOME"))
		if authHome == "" {
			authHome = strings.TrimSpace(os.Getenv("CODEX_HOME"))
		}
		if authHome == "" {
			if home, homeErr := os.UserHomeDir(); homeErr == nil {
				authHome = filepath.Join(home, ".codex")
			}
		}
		if authHome != "" {
			authSource = filepath.Join(authHome, "auth.json")
		}
	}
	if authSource != "" {
		raw, readErr := os.ReadFile(authSource)
		if readErr == nil {
			if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), raw, 0o600); err != nil {
				return "", "", root.ConfigErrorFor(node, "could not copy Codex auth into isolated home: "+err.Error())
			}
		} else if !os.IsNotExist(readErr) {
			return "", "", root.ConfigErrorFor(node, "could not read operator-managed Codex auth: "+readErr.Error())
		}
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil && strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		return "", "", root.ConfigErrorFor(node, "Codex authentication is unavailable; set OV_TEST_CODEX_AUTH_FILE or authenticate Codex on this machine")
	}
	return codexHome, configPath, nil
}

func codexPluginRoot(node string, cfg map[string]any) (string, error) {
	pluginRoot := root.FirstNonEmpty(root.AsString(cfg["codex_plugin_root"]), os.Getenv("OV_TEST_CODEX_PLUGIN_ROOT"))
	if pluginRoot == "" {
		if repo := strings.TrimSpace(os.Getenv("OV_TEST_OPENVIKING_REPO")); repo != "" {
			pluginRoot = filepath.Join(repo, "examples", "codex-memory-plugin")
		}
	}
	if pluginRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			for dir := wd; ; dir = filepath.Dir(dir) {
				candidate := filepath.Join(dir, "OpenViking", "examples", "codex-memory-plugin")
				if st, statErr := os.Stat(filepath.Join(candidate, ".codex-plugin", "plugin.json")); statErr == nil && !st.IsDir() {
					pluginRoot = candidate
					break
				}
				if parent := filepath.Dir(dir); parent == dir {
					break
				}
			}
		}
	}
	if pluginRoot == "" {
		return "", root.ConfigErrorFor(node, "could not resolve Codex OpenViking plugin root; set OV_TEST_OPENVIKING_REPO")
	}
	pluginRoot, err := filepath.Abs(pluginRoot)
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not resolve Codex OpenViking plugin root: "+err.Error())
	}
	pluginRoot, err = filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		return "", root.ConfigErrorFor(node, "could not canonicalize Codex OpenViking plugin root: "+err.Error())
	}
	manifest := filepath.Join(pluginRoot, ".codex-plugin", "plugin.json")
	if st, err := os.Stat(manifest); err != nil || st.IsDir() {
		return "", root.ConfigErrorFor(node, "Codex OpenViking plugin manifest not found at "+manifest)
	}
	return pluginRoot, nil
}

func codexMessage(node string, cfg map[string]any, in map[string]any) (string, error) {
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

func codexOpenVikingEnv(node string, cfg map[string]any, userKey any, cwd string) (map[string]string, map[string]string, string, string, error) {
	conf, _ := root.ReadCLIConf(root.UserBaseConf())
	url := root.FirstNonEmpty(
		root.FirstNonEmpty(root.AsString(cfg["openviking_endpoint"]), root.AsString(cfg["openviking_url"])),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CODEX_OPENVIKING_ENDPOINT"),
			root.FirstNonEmpty(os.Getenv("OV_TEST_CODEX_OPENVIKING_URL"), root.AsString(conf["url"]))),
	)
	if url == "" {
		return nil, nil, "", "", root.ConfigErrorFor(node, fmt.Sprintf("could not resolve OpenViking endpoint from %s", root.UserBaseConf()))
	}

	key := root.FirstNonEmpty(root.AsString(cfg["openviking_api_key"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_CODEX_OPENVIKING_API_KEY"), root.AsString(userKey)))
	if key == "" {
		key = root.AsString(conf["api_key"])
	}
	if key == "" {
		return nil, nil, "", "", root.ConfigErrorFor(node, "Codex OpenViking test requires an API key")
	}

	stateDir := root.FirstNonEmpty(root.AsString(cfg["state_dir"]), os.Getenv("OV_TEST_CODEX_STATE_DIR"))
	if stateDir == "" {
		stateDir = filepath.Join(cwd, ".openviking-codex-state")
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, nil, "", "", root.ConfigErrorFor(node, "could not resolve Codex OpenViking state dir: "+err.Error())
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, nil, "", "", root.ConfigErrorFor(node, "could not create Codex OpenViking state dir: "+err.Error())
	}

	credentialDir := stateDir
	if secretDir := root.SecretStateDir("codex", stateDir); secretDir != "" {
		credentialDir = filepath.Join(secretDir, ctxSafeName(node))
		if err := os.MkdirAll(credentialDir, 0o700); err != nil {
			return nil, nil, "", "", root.ConfigErrorFor(node, "could not create Codex secret state dir: "+err.Error())
		}
	}
	cliConfigPath := filepath.Join(credentialDir, "ovcli.conf")
	cliConfig := map[string]any{
		"url":           strings.TrimRight(url, "/"),
		"api_key":       key,
		"timeout":       120,
		"output":        "json",
		"echo_command":  false,
		"actor_peer_id": root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(cfg["openviking_peer_id"]), os.Getenv("OV_TEST_CODEX_OPENVIKING_PEER_ID")), "codex"),
	}
	body, err := json.MarshalIndent(cliConfig, "", "  ")
	if err != nil {
		return nil, nil, "", "", root.ConfigErrorFor(node, "could not encode Codex ovcli config: "+err.Error())
	}
	if err := os.WriteFile(cliConfigPath, append(body, '\n'), 0o600); err != nil {
		return nil, nil, "", "", root.ConfigErrorFor(node, "could not write Codex ovcli config: "+err.Error())
	}

	openVikingConfigFile := root.FirstNonEmpty(root.FirstNonEmpty(root.AsString(cfg["openviking_config_file"]), os.Getenv("OV_TEST_OPENVIKING_CONF")), os.Getenv("OPENVIKING_CONFIG_FILE"))
	debugLogPath := filepath.Join(stateDir, "codex-hooks.log")
	homeRoot := stateDir
	if credentialDir != stateDir {
		homeRoot = credentialDir
	}
	home := filepath.Join(homeRoot, "home")
	for _, dir := range []string{home, filepath.Join(stateDir, "xdg-config"), filepath.Join(stateDir, "xdg-data"), filepath.Join(stateDir, "xdg-cache"), filepath.Join(stateDir, "xdg-state")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, "", "", root.ConfigErrorFor(node, "could not create Codex isolated home: "+err.Error())
		}
	}
	defaultCLIConfigDir := filepath.Join(home, ".openviking")
	if err := os.MkdirAll(defaultCLIConfigDir, 0o700); err != nil {
		return nil, nil, "", "", root.ConfigErrorFor(node, "could not create Codex default OpenViking config directory: "+err.Error())
	}
	// Codex intentionally sanitizes the environment inherited by plugin hook
	// and MCP subprocesses. Keep the explicit path for shells, and also place
	// the same credential file at the plugin's documented default under the
	// isolated HOME. In release mode HOME lives below the run's secrets tree.
	if err := os.WriteFile(filepath.Join(defaultCLIConfigDir, "ovcli.conf"), append(body, '\n'), 0o600); err != nil {
		return nil, nil, "", "", root.ConfigErrorFor(node, "could not write Codex default OpenViking config: "+err.Error())
	}
	env := map[string]string{
		"HOME":                              home,
		"XDG_CONFIG_HOME":                   filepath.Join(stateDir, "xdg-config"),
		"XDG_DATA_HOME":                     filepath.Join(stateDir, "xdg-data"),
		"XDG_CACHE_HOME":                    filepath.Join(stateDir, "xdg-cache"),
		"XDG_STATE_HOME":                    filepath.Join(stateDir, "xdg-state"),
		"OPENVIKING_CREDENTIAL_SOURCE":      "cli",
		"OPENVIKING_CLI_CONFIG_FILE":        cliConfigPath,
		"OPENVIKING_URL":                    strings.TrimRight(url, "/"),
		"OPENVIKING_BASE_URL":               strings.TrimRight(url, "/"),
		"OPENVIKING_API_KEY":                key,
		"OPENVIKING_AUTH_MODE":              "api_key",
		"OPENVIKING_CODEX_STATE_DIR":        stateDir,
		"OPENVIKING_DEBUG":                  boolEnv(cfg, "debug", "1"),
		"OPENVIKING_DEBUG_LOG":              debugLogPath,
		"OPENVIKING_RECALL_COMPRESS":        boolEnv(cfg, "recall_compress", "0"),
		"OPENVIKING_AUTO_CAPTURE":           boolEnv(cfg, "auto_capture", "1"),
		"OPENVIKING_AUTO_RECALL":            boolEnv(cfg, "auto_recall", "1"),
		"OPENVIKING_CAPTURE_TIMEOUT_MS":     root.FirstNonEmpty(root.AsString(cfg["capture_timeout_ms"]), "120000"),
		"OPENVIKING_RECALL_TIMEOUT_MS":      root.FirstNonEmpty(root.AsString(cfg["recall_timeout_ms"]), "120000"),
		"OPENVIKING_SCORE_THRESHOLD":        root.FirstNonEmpty(root.AsString(cfg["score_threshold"]), "0"),
		"OPENVIKING_RECALL_LIMIT":           root.FirstNonEmpty(root.AsString(cfg["recall_limit"]), "8"),
		"OPENVIKING_CODEX_ACTIVE_WINDOW_MS": root.FirstNonEmpty(root.AsString(cfg["active_window_ms"]), "120000"),
		"OPENVIKING_PENDING_DIR":            filepath.Join(stateDir, "pending"),
		"OPENVIKING_QUEUE_SCOPE_KEY_FILE":   root.QueueScopeKeyFile("codex", stateDir),
		"NO_COLOR":                          "1",
	}
	if peerID := root.FirstNonEmpty(root.AsString(cfg["openviking_peer_id"]), os.Getenv("OV_TEST_CODEX_OPENVIKING_PEER_ID")); peerID != "" {
		env["OPENVIKING_PEER_ID"] = peerID
	}
	if openVikingConfigFile != "" {
		env["OPENVIKING_CONFIG_FILE"] = openVikingConfigFile
	}
	shellEnv := map[string]string{
		"OPENVIKING_CREDENTIAL_SOURCE":      env["OPENVIKING_CREDENTIAL_SOURCE"],
		"OPENVIKING_CLI_CONFIG_FILE":        env["OPENVIKING_CLI_CONFIG_FILE"],
		"OPENVIKING_AUTH_MODE":              env["OPENVIKING_AUTH_MODE"],
		"OPENVIKING_CODEX_STATE_DIR":        env["OPENVIKING_CODEX_STATE_DIR"],
		"OPENVIKING_DEBUG":                  env["OPENVIKING_DEBUG"],
		"OPENVIKING_DEBUG_LOG":              env["OPENVIKING_DEBUG_LOG"],
		"OPENVIKING_RECALL_COMPRESS":        env["OPENVIKING_RECALL_COMPRESS"],
		"OPENVIKING_AUTO_CAPTURE":           env["OPENVIKING_AUTO_CAPTURE"],
		"OPENVIKING_AUTO_RECALL":            env["OPENVIKING_AUTO_RECALL"],
		"OPENVIKING_CAPTURE_TIMEOUT_MS":     env["OPENVIKING_CAPTURE_TIMEOUT_MS"],
		"OPENVIKING_RECALL_TIMEOUT_MS":      env["OPENVIKING_RECALL_TIMEOUT_MS"],
		"OPENVIKING_SCORE_THRESHOLD":        env["OPENVIKING_SCORE_THRESHOLD"],
		"OPENVIKING_RECALL_LIMIT":           env["OPENVIKING_RECALL_LIMIT"],
		"OPENVIKING_CODEX_ACTIVE_WINDOW_MS": env["OPENVIKING_CODEX_ACTIVE_WINDOW_MS"],
		"OPENVIKING_PENDING_DIR":            env["OPENVIKING_PENDING_DIR"],
		"OPENVIKING_QUEUE_SCOPE_KEY_FILE":   env["OPENVIKING_QUEUE_SCOPE_KEY_FILE"],
		"NO_COLOR":                          env["NO_COLOR"],
	}
	if v := env["OPENVIKING_CONFIG_FILE"]; v != "" {
		shellEnv["OPENVIKING_CONFIG_FILE"] = v
	}
	if v := env["OPENVIKING_PEER_ID"]; v != "" {
		shellEnv["OPENVIKING_PEER_ID"] = v
	}
	return env, shellEnv, stateDir, cliConfigPath, nil
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

func writeIsolatedMCPConfig(node string, cfg map[string]any, configPath string, env map[string]string) error {
	pluginRoot, err := codexPluginRoot(node, cfg)
	if err != nil {
		return err
	}
	proxyPath := filepath.Join(pluginRoot, "servers", "mcp-proxy.mjs")
	if st, err := os.Stat(proxyPath); err != nil || st.IsDir() {
		return root.ConfigErrorFor(node, "Codex OpenViking MCP proxy not found at "+proxyPath)
	}
	config := fmt.Sprintf(`[mcp_servers."openviking-memory"]
command = "node"
args = %s
cwd = %s
startup_timeout_sec = 30
env = %s
`, tomlStringList([]string{proxyPath}), tomlQuote(pluginRoot), tomlInlineStringMap(env))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return root.ConfigErrorFor(node, "could not write isolated Codex MCP config: "+err.Error())
	}
	return nil
}

func tomlInlineStringMap(values map[string]string) string {
	var b strings.Builder
	b.WriteByte('{')
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(key)
		b.WriteString(" = ")
		b.WriteString(tomlQuote(values[key]))
	}
	b.WriteByte('}')
	return b.String()
}

func tomlStringList(values []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlQuote(value))
	}
	b.WriteByte(']')
	return b.String()
}

func tomlQuote(value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value)
	return `"` + escaped + `"`
}

func extractLastMessageFromJSONL(jsonl string) string {
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
		if text := codexMessageText(event); text != "" {
			last = text
			continue
		}
		if item, _ := event["item"].(map[string]any); item != nil {
			if text := codexMessageText(item); text != "" {
				last = text
			}
		}
	}
	return last
}

func codexMessageText(event map[string]any) string {
	t := strings.ToLower(root.AsString(event["type"]))
	if t != "agent_message" && t != "assistant" && t != "message" {
		return ""
	}
	if text := root.AsString(event["text"]); text != "" {
		return text
	}
	return root.AsString(event["message"])
}

func ctxSafeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "codex"
	}
	return strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(name)
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
		if isCompletedOpenVikingMCPCall(x) {
			out[canonicalToolName(toolNameFromMap(x))] = true
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

func isCompletedOpenVikingMCPCall(m map[string]any) bool {
	if strings.ToLower(root.AsString(m["type"])) != "mcp_tool_call" {
		return false
	}
	rawTool := toolNameFromMap(m)
	if rawTool == "" || !isOpenVikingToolCall(m, rawTool) {
		return false
	}
	status := strings.ToLower(root.AsString(m["status"]))
	if status != "completed" && status != "success" {
		return false
	}
	if errValue, ok := m["error"]; ok && errValue != nil {
		if s, ok := errValue.(string); !ok || strings.TrimSpace(s) != "" {
			return false
		}
	}
	result, ok := m["result"]
	return ok && result != nil && !sharedevidence.HasBusinessError(result)
}

func observedOpenVikingTools(jsonl string) sharedevidence.ToolSet {
	out := sharedevidence.ToolSet{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if strings.EqualFold(root.AsString(x["type"]), "mcp_tool_call") {
				tool := toolNameFromMap(x)
				if tool != "" && isOpenVikingToolCall(x, tool) {
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
			if isCompletedOpenVikingMCPCall(x) {
				if raw, err := json.Marshal(x["result"]); err == nil {
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

func isOpenVikingToolCall(m map[string]any, tool string) bool {
	if strings.Contains(strings.ToLower(tool), "openviking") {
		return true
	}
	for _, key := range []string{"server", "server_name", "mcp_server", "serverName"} {
		if strings.Contains(strings.ToLower(root.AsString(m[key])), "openviking") {
			return true
		}
	}
	return false
}

func canonicalToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return name
}

func evidenceOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"jsonl", "reply", "after"}, Outputs: []string{"ok", "text", "tools"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			jsonl := root.FirstNonEmpty(root.AsString(in["jsonl"]), root.AsString(ctx.Config()["jsonl"]))
			reply := root.FirstNonEmpty(root.AsString(in["reply"]), root.AsString(ctx.Config()["reply"]))
			text := strings.TrimSpace(successfulMCPResultText(jsonl) + "\n" + reply)
			if strings.TrimSpace(jsonl+"\n"+reply) == "" {
				return nil, ctx.ConfigErr("codex_evidence requires jsonl or reply input")
			}

			missingTools := missingMCPTools(jsonl, tokenList(ctx.Config()["expect_tools"]))
			if len(missingTools) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Codex evidence missing OpenViking MCP tool call(s) %v", missingTools))
			}
			observed := observedOpenVikingTools(jsonl)
			var forbiddenTools []string
			for _, tool := range tokenList(ctx.Config()["forbid_tools"]) {
				if observed.Contains(canonicalToolName(tool)) {
					forbiddenTools = append(forbiddenTools, tool)
				}
			}
			if len(forbiddenTools) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Codex evidence contained forbidden OpenViking MCP tool call(s) %v", forbiddenTools))
			}

			haystack := strings.ToLower(text)
			if missing := root.MissingTokens(haystack, root.LowerAll(tokenList(ctx.Config()["expect"]))); len(missing) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Codex evidence missing expected token(s) %v", missing))
			}
			if leaked := root.PresentTokens(haystack, root.LowerAll(tokenList(ctx.Config()["forbid"]))); len(leaked) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Codex evidence contained forbidden token(s) %v", leaked))
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
