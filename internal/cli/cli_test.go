package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExitDetailFallsBackToJSONStdout(t *testing.T) {
	got := exitDetail(cliResult{ExitCode: 1, Stdout: `{"ok":false,"error":"session missing"}`})
	if !strings.Contains(got, "session missing") {
		t.Fatalf("exitDetail = %q", got)
	}
	got = exitDetail(cliResult{ExitCode: 1, Stdout: "stdout detail", Stderr: "stderr detail"})
	if strings.Contains(got, "stdout detail") || !strings.Contains(got, "stderr detail") {
		t.Fatalf("stderr must remain authoritative: %q", got)
	}
}

func TestMergeEnvExtraOverridesShell(t *testing.T) {
	got := mergeEnv(
		[]string{"OPENVIKING_CLI_CONFIG_FILE=/from-shell", "OPENVIKING_API_KEY=old", "KEEP=1"},
		map[string]string{"OPENVIKING_CLI_CONFIG_FILE": "/root", "OPENVIKING_API_KEY": "fresh"},
	)
	seen := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		if _, ok := seen[k]; ok {
			t.Fatalf("duplicate env key %q in %v", k, got)
		}
		seen[k] = v
	}
	if seen["OPENVIKING_CLI_CONFIG_FILE"] != "/root" || seen["OPENVIKING_API_KEY"] != "fresh" || seen["KEEP"] != "1" {
		t.Fatalf("merged env = %v", got)
	}
}

func TestRunOvUsesConfiguredBinary(t *testing.T) {
	orig := runCLIContext
	defer func() { runCLIContext = orig }()

	var seenArgv []string
	var seenEnv map[string]string
	runCLIContext = func(_ context.Context, argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		seenArgv = append([]string{}, argv...)
		seenEnv = envExtra
		return cliResult{ExitCode: 0}
	}

	t.Setenv("OV_TEST_OV_BIN", "target/debug/ov")
	runOv([]string{"find", "needle"}, "/tmp/user.conf", 0)

	if len(seenArgv) != 5 || seenArgv[0] != "target/debug/ov" ||
		seenArgv[1] != "find" || seenArgv[2] != "needle" || seenArgv[3] != "-o" || seenArgv[4] != "json" {
		t.Fatalf("runOv argv = %v", seenArgv)
	}
	if seenEnv["OPENVIKING_CLI_CONFIG_FILE"] != "/tmp/user.conf" {
		t.Fatalf("runOv env = %v", seenEnv)
	}
}

func TestRunVikingCleanupRefusesImplicitBroadCleanup(t *testing.T) {
	orig := runCLI
	defer func() { runCLI = orig }()

	var seenArgv [][]string
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		seenArgv = append(seenArgv, append([]string{}, argv...))
		return cliResult{ExitCode: 0}
	}

	dir := t.TempDir()
	t.Setenv("OV_TEST_CONF_DIR", dir)
	res := runVikingCleanup()
	if res.ExitCode == 0 || len(seenArgv) != 0 || !strings.Contains(res.Stderr, "explicit run-owned") {
		t.Fatalf("cleanup result=%+v argv=%v", res, seenArgv)
	}
}

func TestRunVikingCleanupUsesExplicitCleanupURIs(t *testing.T) {
	orig := runCLI
	defer func() { runCLI = orig }()

	var deletes [][]string
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		if len(argv) > 1 && argv[1] == "delete" {
			deletes = append(deletes, append([]string{}, argv...))
		}
		return cliResult{ExitCode: 0}
	}

	t.Setenv("OV_TEST_CLEANUP_URIS", "viking://user/memories/item-1, viking://resources/ovtest/run-1")
	runVikingCleanup()

	want := [][]string{
		{"ov", "delete", "-r", "viking://user/memories/item-1"},
		{"ov", "delete", "-r", "viking://resources/ovtest/run-1"},
	}
	if len(deletes) != len(want) {
		t.Fatalf("delete argv = %v", deletes)
	}
	for i := range want {
		if strings.Join(deletes[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("delete argv = %v", deletes)
		}
	}
}

func TestRunVikingCleanupRejectsBroadExplicitTargets(t *testing.T) {
	t.Setenv("OV_TEST_CLEANUP_URIS", "viking://user/alice,viking://resources")
	res := runVikingCleanup()
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "no safe concrete") {
		t.Fatalf("broad cleanup result = %+v", res)
	}
}

func TestRunVikingCleanupRetriesConflict(t *testing.T) {
	origRun, origSleep := runCLI, cleanupSleep
	defer func() { runCLI, cleanupSleep = origRun, origSleep }()

	cleanupSleep = func(_ time.Duration) {}
	t.Setenv("OV_TEST_CLEANUP_URIS", "viking://user/memories/item-1")
	deletes := 0
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		if len(argv) > 1 && argv[1] == "status" {
			return cliResult{ExitCode: 0, Stdout: `{"ok":true,"result":{"user_id":"hermes"}}`}
		}
		if len(argv) > 1 && argv[1] == "wait" {
			return cliResult{ExitCode: 0}
		}
		deletes++
		if deletes == 1 {
			return cliResult{ExitCode: 1, Stderr: "CONFLICT: Resource is being processed: viking://"}
		}
		return cliResult{ExitCode: 0, Stdout: "Removed: viking://"}
	}

	res := runVikingCleanup()
	if res.ExitCode != 0 || deletes != 2 {
		t.Fatalf("cleanup retry result=%+v deletes=%d", res, deletes)
	}
}

func TestRunVikingCleanupTreatsMissingTargetAsClean(t *testing.T) {
	origRun := runCLI
	defer func() { runCLI = origRun }()

	t.Setenv("OV_TEST_CLEANUP_URIS", "viking://user/memories/item-1")
	deletes := 0
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		if len(argv) > 1 && argv[1] == "wait" {
			return cliResult{ExitCode: 0}
		}
		if len(argv) > 1 && argv[1] == "delete" {
			deletes++
			return cliResult{ExitCode: 1, Stderr: "404 not found"}
		}
		return cliResult{ExitCode: 0}
	}

	res := runVikingCleanup()
	if res.ExitCode != 0 || deletes != 1 {
		t.Fatalf("cleanup missing target result=%+v deletes=%d", res, deletes)
	}
}

func TestPreflightOpenVikingUserRepairsStaleLocalKey(t *testing.T) {
	origRun, origBootstrap := runCLI, BootstrapVikingTestUser
	defer func() { runCLI, BootstrapVikingTestUser = origRun, origBootstrap }()

	var seenArgv []string
	var seenEnv map[string]string
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		seenArgv = append([]string{}, argv...)
		seenEnv = envExtra
		return cliResult{ExitCode: 1, Stderr: "Invalid API Key"}
	}
	bootstrapCalls := 0
	BootstrapVikingTestUser = func() (BootstrapResult, error) {
		bootstrapCalls++
		return BootstrapResult{AccountID: "loop-ovtest", UserID: "hermes", UserKeyLength: 111}, nil
	}

	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf"), map[string]any{
		"url": "http://127.0.0.1:1933", "api_key": "stale-user-key",
	})
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf.root"), map[string]any{
		"url": "http://127.0.0.1:1933", "root_api_key": "root-key",
	})
	t.Setenv("OV_TEST_CONF_DIR", dir)

	res, err := PreflightOpenVikingUser()
	if err != nil {
		t.Fatalf("PreflightOpenVikingUser: %v", err)
	}
	if !res.Repaired {
		t.Fatalf("preflight result = %+v, want repaired", res)
	}
	if bootstrapCalls != 1 {
		t.Fatalf("bootstrap calls = %d, want 1", bootstrapCalls)
	}
	wantArgv := []string{"ov", "status", "-o", "json"}
	if strings.Join(seenArgv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("preflight status argv = %v", seenArgv)
	}
	if seenEnv["OPENVIKING_CLI_CONFIG_FILE"] != filepath.Join(dir, "ovcli.conf") {
		t.Fatalf("preflight env = %v", seenEnv)
	}
}

func TestPreflightOpenVikingUserRepairsRejectedAPIKeyStatusMessage(t *testing.T) {
	origRun, origBootstrap := runCLI, BootstrapVikingTestUser
	defer func() { runCLI, BootstrapVikingTestUser = origRun, origBootstrap }()

	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		return cliResult{ExitCode: 1, Stderr: "Authentication Error\nOpenViking rejected the API key for the active config."}
	}
	bootstrapCalls := 0
	BootstrapVikingTestUser = func() (BootstrapResult, error) {
		bootstrapCalls++
		return BootstrapResult{AccountID: "loop-ovtest", UserID: "hermes", UserKeyLength: 111}, nil
	}

	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf"), map[string]any{
		"url": "http://127.0.0.1:1933", "api_key": "stale-user-key",
	})
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf.root"), map[string]any{
		"url": "http://127.0.0.1:1933", "root_api_key": "root-key",
	})
	t.Setenv("OV_TEST_CONF_DIR", dir)

	res, err := PreflightOpenVikingUser()
	if err != nil {
		t.Fatalf("PreflightOpenVikingUser: %v", err)
	}
	if !res.Repaired || bootstrapCalls != 1 {
		t.Fatalf("preflight result=%+v bootstrapCalls=%d, want repaired once", res, bootstrapCalls)
	}
}

func TestPreflightOpenVikingUserSkipsHealthyUser(t *testing.T) {
	origRun, origBootstrap := runCLI, BootstrapVikingTestUser
	defer func() { runCLI, BootstrapVikingTestUser = origRun, origBootstrap }()

	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		return cliResult{ExitCode: 0, Stdout: `{"ok":true,"result":{"user_id":"hermes"}}`}
	}
	BootstrapVikingTestUser = func() (BootstrapResult, error) {
		t.Fatal("bootstrap should not run for a healthy user key")
		return BootstrapResult{}, nil
	}

	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf"), map[string]any{
		"url": "http://localhost:1933", "api_key": "valid-user-key",
	})
	t.Setenv("OV_TEST_CONF_DIR", dir)

	res, err := PreflightOpenVikingUser()
	if err != nil {
		t.Fatalf("PreflightOpenVikingUser: %v", err)
	}
	if res.Repaired {
		t.Fatalf("preflight result = %+v, should not repair", res)
	}
}

func TestPreflightOpenVikingUserSkipsRemoteTargets(t *testing.T) {
	origRun, origBootstrap := runCLI, BootstrapVikingTestUser
	defer func() { runCLI, BootstrapVikingTestUser = origRun, origBootstrap }()

	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		t.Fatal("remote preflight should not call ov")
		return cliResult{}
	}
	BootstrapVikingTestUser = func() (BootstrapResult, error) {
		t.Fatal("remote preflight should not bootstrap")
		return BootstrapResult{}, nil
	}

	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf"), map[string]any{
		"url": "https://openviking.example.com", "api_key": "remote-user-key",
	})
	t.Setenv("OV_TEST_CONF_DIR", dir)

	res, err := PreflightOpenVikingUser()
	if err != nil {
		t.Fatalf("PreflightOpenVikingUser: %v", err)
	}
	if res.Repaired {
		t.Fatalf("preflight result = %+v, should not repair remote targets", res)
	}
}

func TestBootstrapVikingTestUserCreatesAccountAndWritesUserKey(t *testing.T) {
	orig := runCLI
	defer func() { runCLI = orig }()

	var seenArgv []string
	var seenEnv map[string]string
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		seenArgv = append([]string{}, argv...)
		seenEnv = envExtra
		return cliResult{ExitCode: 0, Stdout: `{"status":"ok","result":{"account_id":"loop-ovtest","admin_user_id":"hermes","user_key":"fresh-user-key"}}`}
	}

	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf"), map[string]any{
		"url": "http://127.0.0.1:1933", "api_key": "stale-user-key", "root_api_key": "root-key",
	})
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf.root"), map[string]any{
		"url": "http://127.0.0.1:1933", "api_key": "stale-template-key", "root_api_key": "root-key",
	})
	t.Setenv("OV_TEST_CONF_DIR", dir)
	t.Setenv("OV_TEST_ACCOUNT_ID", "loop-ovtest")
	t.Setenv("OV_TEST_USER_ID", "hermes")
	t.Setenv("OV_TEST_USER_KEY_SEED", "seed-value")

	res, err := bootstrapVikingTestUser()
	if err != nil {
		t.Fatalf("bootstrapVikingTestUser: %v", err)
	}

	wantArgv := []string{"ov", "--sudo", "admin", "create-account", "loop-ovtest", "--admin", "hermes", "--seed", "seed-value", "-o", "json"}
	if strings.Join(seenArgv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("bootstrap argv = %v", seenArgv)
	}
	if seenEnv["OPENVIKING_CLI_CONFIG_FILE"] != filepath.Join(dir, "ovcli.conf.root") {
		t.Fatalf("bootstrap env = %v", seenEnv)
	}
	if res.AccountID != "loop-ovtest" || res.UserID != "hermes" || res.UserKeyLength != len("fresh-user-key") {
		t.Fatalf("bootstrap result = %+v", res)
	}

	for _, path := range []string{filepath.Join(dir, "ovcli.conf"), filepath.Join(dir, "ovcli.conf.root")} {
		conf := readJSONFile(t, path)
		if conf["api_key"] != "fresh-user-key" {
			t.Fatalf("%s api_key = %v, want fresh user key", path, conf["api_key"])
		}
		if conf["root_api_key"] != "root-key" {
			t.Fatalf("%s root_api_key = %v, want preserved root key", path, conf["root_api_key"])
		}
	}
}

func TestBootstrapVikingTestUserRegeneratesExistingUserKey(t *testing.T) {
	orig := runCLI
	defer func() { runCLI = orig }()

	var seen [][]string
	runCLI = func(argv []string, envExtra map[string]string, settle, timeout int) cliResult {
		seen = append(seen, append([]string{}, argv...))
		if len(seen) == 1 {
			return cliResult{ExitCode: 1, Stderr: "account already exists"}
		}
		return cliResult{ExitCode: 0, Stdout: `{"status":"ok","result":{"user_key":"regenerated-key"}}`}
	}

	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "ovcli.conf"), map[string]any{
		"url": "http://127.0.0.1:1933", "api_key": "stale-user-key", "root_api_key": "root-key",
	})
	t.Setenv("OV_TEST_CONF_DIR", dir)

	res, err := bootstrapVikingTestUser()
	if err != nil {
		t.Fatalf("bootstrapVikingTestUser: %v", err)
	}
	if len(seen) != 2 || seen[1][3] != "regenerate-key" {
		t.Fatalf("bootstrap should fall back to regenerate-key, argv=%v", seen)
	}
	if res.UserKeyLength != len("regenerated-key") {
		t.Fatalf("bootstrap result = %+v", res)
	}
	conf := readJSONFile(t, filepath.Join(dir, "ovcli.conf"))
	if conf["api_key"] != "regenerated-key" {
		t.Fatalf("api_key = %v, want regenerated key", conf["api_key"])
	}
}

func TestWriteUserConfDerivesTargetFromRootConf(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ovcli.conf.root")
	if err := os.WriteFile(root, []byte(`{"url":"http://ov.local","root_api_key":"root","account":"a","user":"u","timeout":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_ROOT_CONF", root)
	defer Cleanup()

	path, err := writeUserConf("user-key")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	if conf["url"] != "http://ov.local" || conf["api_key"] != "user-key" || conf["timeout"] != float64(9) {
		t.Fatalf("derived conf = %v", conf)
	}
	if conf["root_api_key"] != nil || conf["account"] != nil || conf["user"] != nil {
		t.Fatalf("derived conf leaked root identity fields: %v", conf)
	}
}

func TestWriteUserConfUsesOpenClawOpenVikingURLOverride(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ovcli.conf.root")
	if err := os.WriteFile(root, []byte(`{"url":"http://ov.local","root_api_key":"root"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_ROOT_CONF", root)
	t.Setenv("OV_TEST_OPENCLAW_OPENVIKING_URL", "http://openclaw-ov.local")
	defer Cleanup()

	path, err := writeUserConf("user-key")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	if conf["url"] != "http://openclaw-ov.local" || conf["api_key"] != "user-key" {
		t.Fatalf("derived conf = %v", conf)
	}
}

func writeJSONFile(t *testing.T, path string, body map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
