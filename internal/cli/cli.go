package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clitool: the one tool-agnostic place ovtest shells out. `ov` and `openclaw`
// ops are thin wrappers over runCLI, so a new tool family is a new wrapper, not
// new process plumbing. runCLI / runOv / writeUserConf are package vars so tests
// can stub them with canned results (no live subprocess).

type cliResult struct {
	Cmd       string
	ExitCode  int
	Stdout    string
	Stderr    string
	DurationS float64
}

// Result is the public subprocess result shape exposed to higher layers.
type Result = cliResult

type BootstrapResult struct {
	AccountID     string
	UserID        string
	UserKeyLength int
	UpdatedPaths  []string
}

type PreflightResult struct {
	Checked       bool
	Repaired      bool
	SkippedReason string
}

// fields spreads the result into an op's output map (the evidence trace shows the
// raw cmd / exit / stdout / stderr / duration for every node).
func (r cliResult) fields() map[string]any {
	return map[string]any{
		"cmd": r.Cmd, "exit_code": r.ExitCode, "stdout": r.Stdout,
		"stderr": r.Stderr, "duration_s": r.DurationS,
	}
}

func exitDetail(r cliResult) string {
	detail := strings.TrimSpace(r.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(r.Stdout)
	}
	return fmt.Sprintf("exit %d: %s", r.ExitCode, detail)
}

func Fields(r Result) map[string]any { return r.fields() }
func ExitDetail(r Result) string     { return exitDetail(r) }

func rootConf() string {
	if v := os.Getenv("OV_TEST_ROOT_CONF"); v != "" {
		return v
	}
	return filepath.Join(confDir(), "ovcli.conf.root")
}

func RootConf() string { return rootConf() }

func confDir() string {
	if v := os.Getenv("OV_TEST_CONF_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openviking")
}

func userBaseConf() string {
	return filepath.Join(confDir(), "ovcli.conf")
}

func UserBaseConf() string { return userBaseConf() }

func userConfTemplate() string {
	if v := os.Getenv("OV_TEST_ROOT_CONF"); v != "" {
		return v
	}
	if _, err := os.Stat(rootConf()); err == nil {
		return rootConf()
	}
	return userBaseConf()
}

func readCLIConf(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		return nil, err
	}
	return conf, nil
}

func ReadConf(path string) (map[string]any, error) { return readCLIConf(path) }

func targetURL() (string, error) {
	path := userBaseConf()
	conf, err := readCLIConf(path)
	if err != nil {
		return "", err
	}
	url := asString(conf["url"])
	if url == "" {
		return "", fmt.Errorf("%s has no url", path)
	}
	return url, nil
}

func TargetURL() (string, error) { return targetURL() }

func targetAPIKey() (string, error) {
	path := userBaseConf()
	conf, err := readCLIConf(path)
	if err != nil {
		return "", err
	}
	key := asString(conf["api_key"])
	if key == "" {
		return "", fmt.Errorf("%s has no api_key", path)
	}
	return key, nil
}

func TargetAPIKey() (string, error) { return targetAPIKey() }

func ovBin() string {
	if v := os.Getenv("OV_TEST_OV_BIN"); v != "" {
		return v
	}
	return "ov"
}

// runCLI runs any CLI subprocess, returning a uniform result. `settle` sleeps
// first (for polling eventually-consistent targets); `timeout` (seconds, 0 = no
// limit) guards a hung child — on a kill the partial stdout/stderr is preserved
// (that is where the real cause lives) and the exit code is 124.
func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := extra[k]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

var runCLIContext = func(parent context.Context, argv []string, envExtra map[string]string, settle, timeout int) cliResult {
	if parent == nil {
		parent = context.Background()
	}
	t0 := time.Now()
	if settle > 0 {
		timer := time.NewTimer(time.Duration(settle) * time.Second)
		select {
		case <-timer.C:
		case <-parent.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return cliResult{Cmd: strings.Join(argv, " "), ExitCode: 124,
				Stderr: parent.Err().Error(), DurationS: round2(time.Since(t0).Seconds())}
		}
	}
	ctx := parent
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	prepareCommand(cmd)
	cmd.Cancel = func() error { return cancelCommand(cmd) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = mergeEnv(os.Environ(), envExtra)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	dur := round2(time.Since(t0).Seconds())

	res := cliResult{Cmd: strings.Join(argv, " "), Stdout: stdout.String(),
		Stderr: stderr.String(), DurationS: dur}
	if ctx.Err() == context.DeadlineExceeded {
		res.ExitCode = 124
		res.Stderr = strings.TrimSpace(res.Stderr + fmt.Sprintf("\n[timeout after %ds]", timeout))
		return res
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = 127 // could not launch (e.g. command not found)
			if res.Stderr == "" {
				res.Stderr = err.Error()
			}
		}
	}
	return res
}

var runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
	return runCLIContext(context.Background(), argv, envExtra, settle, timeout)
}

var Run = func(argv []string, envExtra map[string]string, settle, timeout int) Result {
	return runCLI(argv, envExtra, settle, timeout)
}

var RunContext = func(ctx context.Context, argv []string, envExtra map[string]string, settle, timeout int) Result {
	return runCLIContext(ctx, argv, envExtra, settle, timeout)
}

// runOv runs `ov <args> -o json` as the identity in confPath.
var runOvContext = func(ctx context.Context, args []string, confPath string, settle int) cliResult {
	timeout := envInt("OV_TEST_CLI_TIMEOUT", 300)
	argv := append([]string{ovBin()}, args...)
	argv = append(argv, "-o", "json")
	return runCLIContext(ctx, argv, map[string]string{"OPENVIKING_CLI_CONFIG_FILE": confPath}, settle, timeout)
}

var runOv = func(args []string, confPath string, settle int) cliResult {
	return runOvContext(context.Background(), args, confPath, settle)
}

var RunOv = func(args []string, confPath string, settle int) Result {
	return runOv(args, confPath, settle)
}

var RunOvContext = func(ctx context.Context, args []string, confPath string, settle int) Result {
	return runOvContext(ctx, args, confPath, settle)
}

var cleanupSleep = time.Sleep

func cleanupTargets(env map[string]string, timeout int) ([]string, cliResult) {
	if explicit := os.Getenv("OV_TEST_CLEANUP_URIS"); explicit != "" {
		targets := normalizeCleanupTargets(strings.Split(explicit, ","))
		if len(targets) == 0 {
			return nil, cliResult{ExitCode: 2, Stderr: "OV_TEST_CLEANUP_URIS contains no safe concrete targets"}
		}
		return targets, cliResult{}
	}
	return nil, cliResult{ExitCode: 2, Stderr: "API cleanup requires explicit run-owned OV_TEST_CLEANUP_URIS"}
}

func normalizeCleanupTargets(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		uri := strings.TrimSpace(item)
		uri = strings.TrimRight(uri, "/")
		if uri == "" || uri == "viking:/" {
			continue
		}
		if uri == "viking://" || uri == "viking://user" || uri == "viking://agent" || uri == "viking://resources" {
			continue
		}
		parsed, err := url.Parse(uri)
		if err != nil || parsed == nil {
			continue
		}
		segments := strings.FieldsFunc(parsed.EscapedPath(), func(r rune) bool { return r == '/' })
		if parsed.Scheme != "viking" || len(segments) < 2 || seen[uri] {
			continue
		}
		seen[uri] = true
		out = append(out, uri)
	}
	return out
}

func userIDFromStatus(stdout string) string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return ""
	}
	if user := asString(parsed["user_id"]); user != "" {
		return user
	}
	result, _ := parsed["result"].(map[string]any)
	return asString(result["user_id"])
}

func cleanupTarget(target string, env map[string]string, timeout int) cliResult {
	var last cliResult
	for i := 0; i < 5; i++ {
		wait := runCLI([]string{ovBin(), "wait", "--timeout", "120"}, env, 0, timeout)
		if wait.ExitCode != 0 {
			return wait
		}
		last = runCLI([]string{ovBin(), "delete", "-r", target}, env, 0, timeout)
		if isCleanupNotFound(last) {
			last.ExitCode = 0
			return last
		}
		if last.ExitCode == 0 || !strings.Contains(last.Stderr, "CONFLICT") {
			return last
		}
		cleanupSleep(2 * time.Second)
	}
	return last
}

func isCleanupNotFound(r cliResult) bool {
	text := strings.ToLower(r.Stdout + "\n" + r.Stderr)
	return strings.Contains(text, "not_found") || strings.Contains(text, "not found") || strings.Contains(text, "404")
}

var runVikingCleanup = func() cliResult {
	timeout := envInt("OV_TEST_CLI_TIMEOUT", 300)
	conf := userBaseConf()
	env := map[string]string{"OPENVIKING_CLI_CONFIG_FILE": conf}
	targets, failed := cleanupTargets(env, timeout)
	if failed.Cmd != "" || failed.ExitCode != 0 {
		return failed
	}
	if len(targets) == 0 {
		return cliResult{ExitCode: 0, Stdout: "No cleanup targets resolved"}
	}
	var last cliResult
	for _, target := range targets {
		last = cleanupTarget(target, env, timeout)
		if last.ExitCode != 0 && !isCleanupNotFound(last) {
			return last
		}
	}
	return last
}

var RunVikingCleanup = func() Result {
	return runVikingCleanup()
}

var BootstrapVikingTestUser = func() (BootstrapResult, error) {
	return bootstrapVikingTestUser()
}

var PreflightOpenVikingUser = func() (PreflightResult, error) {
	return preflightOpenVikingUser()
}

func preflightOpenVikingUser() (PreflightResult, error) {
	conf, err := readCLIConf(userBaseConf())
	if os.IsNotExist(err) {
		return PreflightResult{SkippedReason: "missing user config"}, nil
	}
	if err != nil {
		return PreflightResult{}, err
	}
	target := asString(conf["url"])
	if target == "" {
		return PreflightResult{}, fmt.Errorf("%s has no url", userBaseConf())
	}
	if !isLocalOpenVikingURL(target) {
		return PreflightResult{SkippedReason: "non-local target"}, nil
	}

	timeout := envInt("OV_TEST_CLI_TIMEOUT", 300)
	env := map[string]string{"OPENVIKING_CLI_CONFIG_FILE": userBaseConf()}
	status := runCLI([]string{ovBin(), "status", "-o", "json"}, env, 0, timeout)
	if status.ExitCode == 0 {
		return PreflightResult{Checked: true}, nil
	}
	if !isAuthFailure(status) {
		return PreflightResult{Checked: true, SkippedReason: "status did not fail with auth error"}, nil
	}
	if !bootstrapConfigAvailable() {
		return PreflightResult{Checked: true}, fmt.Errorf("local OpenViking user key is invalid, but no root config is available for automatic repair")
	}
	if _, err := BootstrapVikingTestUser(); err != nil {
		return PreflightResult{Checked: true}, err
	}
	return PreflightResult{Checked: true, Repaired: true}, nil
}

func isLocalOpenVikingURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isAuthFailure(r cliResult) bool {
	text := strings.ToLower(r.Stdout + "\n" + r.Stderr)
	return r.ExitCode == 401 ||
		strings.Contains(text, "invalid api key") ||
		strings.Contains(text, "rejected the api key") ||
		strings.Contains(text, "authentication error") ||
		strings.Contains(text, "unauthenticated") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "401")
}

func bootstrapConfigAvailable() bool {
	if root := rootConf(); root != userBaseConf() {
		if _, err := os.Stat(root); err == nil {
			return true
		}
	}
	conf, err := readCLIConf(userBaseConf())
	return err == nil && asString(conf["root_api_key"]) != ""
}

func bootstrapVikingTestUser() (BootstrapResult, error) {
	accountID := envStr("OV_TEST_ACCOUNT_ID", "loop-ovtest")
	userID := envStr("OV_TEST_USER_ID", "hermes")
	seed := envStr("OV_TEST_USER_KEY_SEED", accountID+"-"+userID)
	adminConf := adminBootstrapConf()
	timeout := envInt("OV_TEST_CLI_TIMEOUT", 300)
	env := map[string]string{"OPENVIKING_CLI_CONFIG_FILE": adminConf}

	create := []string{ovBin(), "--sudo", "admin", "create-account", accountID, "--admin", userID, "--seed", seed, "-o", "json"}
	res := runCLI(create, env, 0, timeout)
	if res.ExitCode != 0 && strings.Contains(strings.ToLower(res.Stdout+"\n"+res.Stderr), "already exists") {
		regen := []string{ovBin(), "--sudo", "admin", "regenerate-key", accountID, userID, "--seed", seed, "-o", "json"}
		res = runCLI(regen, env, 0, timeout)
	}
	if res.ExitCode != 0 {
		return BootstrapResult{}, fmt.Errorf("bootstrap OpenViking test user failed: %s", exitDetail(res))
	}
	userKey, err := userKeyFromAdminResult(res.Stdout)
	if err != nil {
		return BootstrapResult{}, err
	}
	updated, err := updateBootstrapConfigs(userKey)
	if err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{
		AccountID:     accountID,
		UserID:        userID,
		UserKeyLength: len(userKey),
		UpdatedPaths:  updated,
	}, nil
}

func adminBootstrapConf() string {
	if v := os.Getenv("OV_TEST_ROOT_CONF"); v != "" {
		return v
	}
	if _, err := os.Stat(rootConf()); err == nil {
		return rootConf()
	}
	return userBaseConf()
}

func userKeyFromAdminResult(stdout string) (string, error) {
	v, err := resultMap(stdout)
	if err != nil {
		return "", fmt.Errorf("bootstrap OpenViking user output not JSON: %w", err)
	}
	key := asString(v["user_key"])
	if key == "" {
		return "", fmt.Errorf("bootstrap OpenViking user response missing result.user_key")
	}
	return key, nil
}

func updateBootstrapConfigs(userKey string) ([]string, error) {
	paths := []string{userBaseConf()}
	if root := rootConf(); root != userBaseConf() {
		if _, err := os.Stat(root); err == nil {
			paths = append(paths, root)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	base := map[string]any{}
	for _, path := range paths {
		conf, err := readOrBootstrapConf(path, base)
		if err != nil {
			return nil, err
		}
		if url := asString(conf["url"]); url != "" {
			base["url"] = url
		}
		if rootKey := asString(conf["root_api_key"]); rootKey != "" {
			base["root_api_key"] = rootKey
		}
		conf["api_key"] = userKey
		conf["output"] = "table"
		conf["echo_command"] = false
		if err := writeCLIConf(path, conf); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func readOrBootstrapConf(path string, fallback map[string]any) (map[string]any, error) {
	conf, err := readCLIConf(path)
	if err == nil {
		return conf, nil
	}
	if os.IsNotExist(err) && len(fallback) > 0 {
		out := map[string]any{}
		for k, v := range fallback {
			out[k] = v
		}
		return out, nil
	}
	return nil, err
}

func writeCLIConf(path string, conf map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o600)
}

// ── per-user temp conf (cached, cleaned up via Cleanup) ─────────────────────--

var (
	userConfMu    sync.Mutex
	userConfCache = map[string]string{}
)

var writeUserConf = func(key string) (string, error) {
	userConfMu.Lock()
	defer userConfMu.Unlock()
	template := userConfTemplate()
	overrideURL := os.Getenv("OV_TEST_OPENCLAW_OPENVIKING_URL")
	cacheKey := template + "\x00" + overrideURL + "\x00" + key
	if p, ok := userConfCache[cacheKey]; ok {
		return p, nil
	}
	body, err := readCLIConf(template)
	if err != nil {
		return "", err
	}
	if asString(body["url"]) == "" {
		return "", fmt.Errorf("%s has no url", template)
	}
	if overrideURL != "" {
		body["url"] = overrideURL
	}
	for _, k := range []string{"account", "user", "root_api_key"} {
		delete(body, k)
	}
	body["api_key"] = key
	body["output"] = "table"
	body["echo_command"] = false

	f, err := os.CreateTemp("", "ovtest-dag-*.conf")
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(body)
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	userConfCache[cacheKey] = f.Name()
	return f.Name(), nil
}

var WriteUserConf = func(key string) (string, error) {
	return writeUserConf(key)
}

// Cleanup removes the cached per-user conf files. The run CLI defers it.
func Cleanup() {
	userConfMu.Lock()
	defer userConfMu.Unlock()
	for _, p := range userConfCache {
		_ = os.Remove(p)
	}
	userConfCache = map[string]string{}
}

// ── JSON helpers (tolerate ov's leading `cmd: …` echo) ──────────────────────--

func parseJSON(stdout string) (any, error) {
	if i := strings.IndexByte(stdout, '{'); i >= 0 {
		stdout = stdout[i:]
	}
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// resultOf returns the "result" field of an ov JSON response. A valid-but-non-
// object top level (a bare array/scalar) mirrors Python's `(parsed or {}).get`:
// a TRUTHY non-dict is a schema error (so jsonResult HARD-fails with "... output
// not JSON"); a falsy one ([]/0/""/false/null) yields no result, no error.
func resultOf(stdout string) (any, error) {
	v, err := parseJSON(stdout)
	if err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		return m["result"], nil
	}
	if jsonTruthy(v) {
		return nil, fmt.Errorf("top-level JSON is not an object")
	}
	return nil, nil
}

func jsonTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// resultMap returns the "result" object as a map (empty if absent/not an object).
func resultMap(stdout string) (map[string]any, error) {
	r, err := resultOf(stdout)
	if err != nil {
		return nil, err
	}
	if m, ok := r.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

// memoriesOf parses result.memories into [{uri,score,abstract}] plus a parse-error
// string (empty if clean) — so a parse failure lands in the evidence trace rather
// than being silently swallowed as an empty list.
func memoriesOf(stdout string) ([]map[string]any, string) {
	m, err := resultMap(stdout)
	if err != nil {
		return nil, "memories JSON parse failed: " + err.Error()
	}
	rawValue, ok := m["memories"]
	if !ok {
		return nil, "memories JSON parse failed: result.memories missing"
	}
	raw, ok := rawValue.([]any)
	if !ok {
		return nil, "memories JSON parse failed: result.memories is not an array"
	}
	out := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		mm, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("memories JSON parse failed: result.memories[%d] is not an object", i)
		}
		out = append(out, map[string]any{
			"uri": mm["uri"], "score": mm["score"], "abstract": mm["abstract"]})
	}
	return out, ""
}

// ── small coercion helpers ( all CLI/JSON data is untyped any) ──────────────--

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func EnvStr(key, def string) string { return envStr(key, def) }

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func EnvInt(key string, def int) int { return envInt(key, def) }

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n
		}
	}
	return def
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func asStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, asString(e))
		}
		return out
	}
	return nil
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
