package hermes

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// hermes: drive Hermes' normal CLI chat path and let its OpenViking memory
// plugin perform sync_turn/session-end commit. The harness only observes the
// reported session id; it does not write memory directly.

var hermesSessionIDLine = regexp.MustCompile(`(?m)^\s*session_id:\s*(\S+)\s*$`)
var hermesTemplateConfigFiles = []string{"config.yaml", ".env"}

var (
	Chat     = chatOp()
	Evidence = evidenceOp()
)

func hermesBin() string {
	if v := os.Getenv("OV_TEST_HERMES_BIN"); v != "" {
		return v
	}
	return "hermes"
}

func parseHermesSessionID(out string) string {
	m := hermesSessionIDLine.FindStringSubmatch(out)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func chatOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"user_key", "after", "resource_path", "resource_url", "memory_uri", "case_id"},
		Outputs: []string{"reply", "session_id", "home"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			message, err := hermesMessage(ctx.Name(), ctx.Config(), in)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(message) == "" {
				return nil, ctx.ConfigErr("hermes_chat requires non-empty message")
			}
			home := root.FirstNonEmpty(root.AsString(ctx.Config()["home"]), os.Getenv("OV_TEST_HERMES_HOME"))
			if home == "" {
				return nil, ctx.ConfigErr("hermes_chat requires config 'home' or OV_TEST_HERMES_HOME")
			}
			home, err = filepath.Abs(home)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Hermes home: " + err.Error())
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				return nil, ctx.ConfigErr("could not create Hermes home: " + err.Error())
			}
			if err := prepareHermesHome(ctx.Name(), ctx.Config(), home); err != nil {
				return nil, err
			}
			envExtra, err := hermesOpenVikingEnv(ctx.Name(), ctx.Config(), in["user_key"], home)
			if err != nil {
				return nil, err
			}

			argv := []string{hermesBin(), "--cli", "chat", "-q", message, "--quiet"}
			if rawToolsets, ok := ctx.Config()["toolsets"]; ok {
				if toolsets := hermesToolsetsArg(rawToolsets); toolsets != "" {
					argv = append(argv, "--toolsets", toolsets)
				}
			}
			r := ctx.RunCLI(argv, envExtra, 0, root.AsInt(ctx.Config()["timeout"], root.EnvInt("OV_TEST_HERMES_TIMEOUT", 300)))
			if r.ExitCode != 0 {
				detail := root.ExitDetail(r)
				if diagnostic := lastHermesError(home); diagnostic != "" {
					detail += "\nHermes errors.log: " + diagnostic
				}
				return nil, ctx.GateErr(detail)
			}
			reply := strings.TrimSpace(r.Stdout)
			if reply == "" {
				return nil, ctx.GateErr("hermes chat produced an empty reply")
			}
			sessionID := parseHermesSessionID(r.Stderr + "\n" + r.Stdout)
			if sessionID == "" {
				return nil, ctx.GateErr("hermes chat did not report session_id")
			}

			out := root.CLIFields(r)
			out["reply"], out["session_id"], out["home"] = reply, sessionID, home
			return out, nil
		}
	})
}

func lastHermesError(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, "logs", "errors.log"))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return root.Truncate(line, 1200)
		}
	}
	return ""
}

func hermesMessage(node string, cfg map[string]any, in map[string]any) (string, error) {
	if tmpl := root.AsString(cfg["message_template"]); tmpl != "" {
		return renderInputTemplate(tmpl, in), nil
	}
	v, ok := cfg["message"]
	if !ok || v == nil {
		return "", root.ConfigErrorFor(node, "missing required config 'message'")
	}
	return root.AsString(v), nil
}

func renderInputTemplate(tmpl string, in map[string]any) string {
	out := tmpl
	for key, value := range in {
		out = strings.ReplaceAll(out, "{{"+key+"}}", root.AsString(value))
	}
	return out
}

func evidenceOp() dag.Factory {
	return root.NewFactory(dag.Meta{Inputs: []string{"home", "after"}, Outputs: []string{"ok", "text"}}, false, func(ctx *root.OpContext) root.ExecFunc {
		return func(in map[string]any) (map[string]any, error) {
			home := root.FirstNonEmpty(root.AsString(in["home"]), root.AsString(ctx.Config()["home"]))
			if home == "" {
				return nil, ctx.ConfigErr("hermes_evidence requires input/config 'home'")
			}
			home, err := filepath.Abs(home)
			if err != nil {
				return nil, ctx.ConfigErr("could not resolve Hermes home: " + err.Error())
			}
			text, tools, attempted, err := readHermesEvidence(home)
			if err != nil {
				return nil, ctx.ConfigErr(err.Error())
			}
			expected := tokenList(ctx.Config()["expect_tools"])
			if len(expected) == 0 {
				expected = tokenList(ctx.Config()["expect"])
			}
			var missing []string
			for _, tool := range expected {
				if !tools.Contains(tool) {
					missing = append(missing, tool)
				}
			}
			if len(missing) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Hermes evidence missing successful tool result(s) %v", missing))
			}
			forbidden := tokenList(ctx.Config()["forbid_tools"])
			if len(forbidden) == 0 {
				forbidden = tokenList(ctx.Config()["forbid"])
			}
			var present []string
			for _, tool := range forbidden {
				if attempted.Contains(tool) {
					present = append(present, tool)
				}
			}
			if len(present) > 0 {
				return nil, ctx.GateErr(fmt.Sprintf("Hermes evidence contained forbidden successful tool result(s) %v", present))
			}
			return map[string]any{"ok": true, "text": text}, nil
		}
	})
}

func hermesToolsetsArg(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case []string:
		return strings.Join(trimNonEmpty(x), ",")
	case []any:
		items := make([]string, 0, len(x))
		for _, item := range x {
			if s := strings.TrimSpace(root.AsString(item)); s != "" {
				items = append(items, s)
			}
		}
		return strings.Join(items, ",")
	}
	return ""
}

func tokenList(v any) []string {
	if s := strings.TrimSpace(root.AsString(v)); s != "" {
		return []string{s}
	}
	return trimNonEmpty(root.AsStrings(v))
}

func trimNonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func readHermesEvidence(home string) (string, sharedevidence.ToolSet, sharedevidence.ToolSet, error) {
	if info, err := os.Stat(home); err != nil {
		return "", nil, nil, fmt.Errorf("could not read Hermes home %s: %w", home, err)
	} else if !info.IsDir() {
		return "", nil, nil, fmt.Errorf("Hermes home is not a directory: %s", home)
	}
	return readHermesStateToolEvidence(home)
}

var queryHermesState = func(dbPath string) ([]byte, error) {
	python, err := hermesPythonBin()
	if err != nil {
		return nil, err
	}
	const script = `import json, sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.row_factory = sqlite3.Row
rows = db.execute("""
SELECT id, role, COALESCE(content,'') AS content,
       COALESCE(tool_call_id,'') AS tool_call_id,
       COALESCE(tool_calls,'') AS tool_calls,
       COALESCE(tool_name,'') AS tool_name
FROM messages
WHERE active = 1
ORDER BY id
""").fetchall()
print(json.dumps([dict(row) for row in rows], separators=(',', ':')))
`
	cmd := osexec.Command(python, "-c", script, dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Python sqlite3 query failed: %s", root.Truncate(strings.TrimSpace(string(out)), 400))
	}
	return out, nil
}

func hermesPythonBin() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("OV_TEST_HERMES_PYTHON")); configured != "" {
		return configured, nil
	}
	if bin := strings.TrimSpace(os.Getenv("OV_TEST_HERMES_BIN")); bin != "" && filepath.IsAbs(bin) {
		for _, name := range []string{"python3", "python"} {
			candidate := filepath.Join(filepath.Dir(bin), name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	if python, err := osexec.LookPath("python3"); err == nil {
		return python, nil
	}
	return "", fmt.Errorf("Python 3 with stdlib sqlite3 is required for structured Hermes tool evidence; set OV_TEST_HERMES_PYTHON")
}

type hermesEvidenceRow struct {
	ID         int64  `json:"id"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  string `json:"tool_calls"`
	ToolName   string `json:"tool_name"`
}

func readHermesStateToolEvidence(home string) (string, sharedevidence.ToolSet, sharedevidence.ToolSet, error) {
	dbPath := filepath.Join(home, "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "", sharedevidence.ToolSet{}, sharedevidence.ToolSet{}, nil
		}
		return "", nil, nil, fmt.Errorf("could not inspect Hermes state database %s: %w", dbPath, err)
	}
	out, err := queryHermesState(dbPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("could not query Hermes state database %s: %w", dbPath, err)
	}
	return parseHermesEvidenceRows(out)
}

func parseHermesEvidenceRows(raw []byte) (string, sharedevidence.ToolSet, sharedevidence.ToolSet, error) {
	var rows []hermesEvidenceRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", nil, nil, fmt.Errorf("could not decode Hermes state evidence: %w", err)
	}
	calls := map[string]string{}
	observed := sharedevidence.ToolSet{}
	for _, row := range rows {
		if strings.TrimSpace(row.ToolCalls) == "" {
			continue
		}
		var value any
		if json.Unmarshal([]byte(row.ToolCalls), &value) != nil {
			continue
		}
		collectHermesToolCalls(value, calls, observed)
	}
	successful := sharedevidence.ToolSet{}
	var results []string
	for _, row := range rows {
		if !strings.EqualFold(row.Role, "tool") {
			continue
		}
		tool := calls[row.ToolCallID]
		if tool == "" {
			tool = row.ToolName
		}
		if tool == "" || !strings.HasPrefix(strings.ToLower(tool), "viking_") || sharedevidence.HasBusinessError(decodeJSONString(row.Content)) {
			continue
		}
		successful.Add(tool)
		results = append(results, row.Content)
	}
	return strings.Join(results, "\n"), successful, observed, nil
}

func collectHermesToolCalls(v any, calls map[string]string, observed sharedevidence.ToolSet) {
	switch x := v.(type) {
	case map[string]any:
		name := root.AsString(x["name"])
		if fn, _ := x["function"].(map[string]any); name == "" {
			name = root.AsString(fn["name"])
		}
		id := root.FirstNonEmpty(root.AsString(x["id"]), root.AsString(x["call_id"]))
		if id != "" && strings.HasPrefix(strings.ToLower(name), "viking_") {
			calls[id] = name
			observed.Add(name)
		}
		for _, child := range x {
			collectHermesToolCalls(child, calls, observed)
		}
	case []any:
		for _, child := range x {
			collectHermesToolCalls(child, calls, observed)
		}
	}
}

func decodeJSONString(value string) any {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	return value
}

func prepareHermesHome(node string, cfg map[string]any, home string) error {
	templateHome := root.FirstNonEmpty(root.AsString(cfg["home_template"]), os.Getenv("OV_TEST_HERMES_HOME_TEMPLATE"))
	if templateHome == "" {
		return nil
	}
	templateHome, err := filepath.Abs(templateHome)
	if err != nil {
		return root.ConfigErrorFor(node, "could not resolve Hermes home template: "+err.Error())
	}
	info, err := os.Stat(templateHome)
	if err != nil {
		return root.ConfigErrorFor(node, "could not read Hermes home template: "+err.Error())
	}
	if !info.IsDir() {
		return root.ConfigErrorFor(node, "Hermes home template must be a directory")
	}
	for _, name := range hermesTemplateConfigFiles {
		src := filepath.Join(templateHome, name)
		dst := filepath.Join(home, name)
		if err := copyHermesTemplateFile(src, dst); err != nil {
			return root.ConfigErrorFor(node, err.Error())
		}
	}
	return nil
}

func copyHermesTemplateFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("could not read Hermes template file %s: %w", src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Hermes template file %s must not be a symlink", src)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Hermes template file %s must be a regular file", src)
	}
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not inspect Hermes home file %s: %w", dst, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("could not open Hermes template file %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("could not create Hermes home file %s: %w", dst, err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("could not copy Hermes template file %s: %w", src, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("could not close Hermes home file %s: %w", dst, closeErr)
	}
	return nil
}

func hermesOpenVikingEnv(node string, cfg map[string]any, userKey any, home string) (map[string]string, error) {
	conf, _ := root.ReadCLIConf(root.UserBaseConf())
	url := root.FirstNonEmpty(
		root.FirstNonEmpty(root.AsString(cfg["openviking_endpoint"]), root.AsString(cfg["openviking_url"])),
		root.FirstNonEmpty(os.Getenv("OV_TEST_HERMES_OPENVIKING_ENDPOINT"), root.AsString(conf["url"])),
	)
	if url == "" {
		return nil, root.ConfigErrorFor(node, fmt.Sprintf("could not resolve OpenViking endpoint from %s", root.UserBaseConf()))
	}

	key := root.FirstNonEmpty(root.AsString(cfg["openviking_api_key"]),
		root.FirstNonEmpty(os.Getenv("OV_TEST_HERMES_OPENVIKING_API_KEY"), root.AsString(userKey)))
	if key == "" {
		key = root.AsString(conf["api_key"])
	}

	account := root.FirstNonEmpty(root.AsString(cfg["openviking_account"]), os.Getenv("OV_TEST_HERMES_OPENVIKING_ACCOUNT"))
	user := root.FirstNonEmpty(root.AsString(cfg["openviking_user"]), os.Getenv("OV_TEST_HERMES_OPENVIKING_USER"))
	if key == "" {
		account = root.FirstNonEmpty(account, root.AsString(conf["account"]))
		user = root.FirstNonEmpty(user, root.AsString(conf["user"]))
		if account == "" || user == "" {
			return nil, root.ConfigErrorFor(node, "OpenViking auth requires api_key or both account and user")
		}
	}

	env := map[string]string{
		"HOME":                home,
		"HERMES_HOME":         home,
		"OPENVIKING_ENDPOINT": url,
		"OPENVIKING_AGENT":    root.FirstNonEmpty(root.AsString(cfg["openviking_agent"]), root.FirstNonEmpty(os.Getenv("OV_TEST_HERMES_OPENVIKING_AGENT"), "hermes")),
	}
	if key != "" {
		env["OPENVIKING_API_KEY"] = key
	}
	if account != "" {
		env["OPENVIKING_ACCOUNT"] = account
	}
	if user != "" {
		env["OPENVIKING_USER"] = user
	}
	trace := root.FirstNonEmpty(root.AsString(cfg["openviking_sync_trace"]), root.FirstNonEmpty(os.Getenv("OV_TEST_HERMES_SYNC_TRACE"), "1"))
	if trace != "" {
		env["HERMES_OPENVIKING_SYNC_TRACE"] = trace
	}
	return env, nil
}
