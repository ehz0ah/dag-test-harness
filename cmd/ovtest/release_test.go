package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"code.byted.org/data-arch/ovtest/cases"
	"code.byted.org/data-arch/ovtest/internal/releasegate"
	"code.byted.org/data-arch/ovtest/ops"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestParseReleaseArgsDefaultsToPrimarySuite(t *testing.T) {
	t.Setenv("OV_TEST_ENV_FILE", "/tmp/release.env")
	opts, err := parseReleaseArgs([]string{"--root", t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if opts.SourceMode != releasegate.SourceLatest || opts.TargetMode != releasegate.TargetLocal {
		t.Fatalf("modes = %q/%q", opts.SourceMode, opts.TargetMode)
	}
	if got, want := opts.CaseIDs, cases.DefaultNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default cases = %v, want %v", got, want)
	}
	if len(opts.CaseIDs) != 21 {
		t.Fatalf("primary release suite has %d cases, want 21", len(opts.CaseIDs))
	}
}

func TestDefaultReleaseEnvFileNeverDiscoversWorkingDirectoryDotEnv(t *testing.T) {
	t.Setenv("OV_TEST_ENV_FILE", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=ambient\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if got := defaultReleaseEnvFile(); got != "" {
		t.Fatalf("defaultReleaseEnvFile discovered %q", got)
	}
}

func TestParseReleaseArgsRejectsInvalidModeCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"--source", "manifest"},
		{"--source", "latest", "--manifest", "old.json"},
		{"--target", "external"},
		{"--target", "external", "--url", "https://user:secret@example.com"},
		{"--target", "external", "--url", "https://example.com?token=secret"},
		{"--target", "container"},
		{"--retention-days", "0"},
		{"--qualification-runs", "0"},
		{"--qualification-runs", "21"},
		{"unknown-case"},
		{"--suite", "codex", "codex-openviking-automatic-memory"},
		{"--suite", "unknown"},
	} {
		if _, err := parseReleaseArgs(args); err == nil {
			t.Fatalf("parseReleaseArgs(%v) should fail", args)
		}
	}
}

func TestReleaseHelpSucceedsWithoutExecution(t *testing.T) {
	if code := runRelease([]string{"--help"}); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
}

func TestHarnessPreflightsFollowSelectedCases(t *testing.T) {
	all := harnessPreflightCases(releaseOptions{CaseIDs: cases.DefaultNames()}, t.TempDir())
	if len(all) != 6 {
		t.Fatalf("all harness preflights = %d, want 6", len(all))
	}
	selected := harnessPreflightCases(releaseOptions{CaseIDs: []string{
		"openviking-service-baseline", "codex-openviking-automatic-memory",
	}}, t.TempDir())
	if len(selected) != 1 || selected[0].Harness != "codex" {
		t.Fatalf("selected preflights = %+v", selected)
	}
	if baseline := harnessPreflightCases(releaseOptions{CaseIDs: []string{"openviking-service-baseline"}}, t.TempDir()); len(baseline) != 0 {
		t.Fatalf("baseline-only preflights = %+v", baseline)
	}
}

func TestParseReleaseArgsSelectsNamedHarnessSuite(t *testing.T) {
	opts, err := parseReleaseArgs([]string{"--suite", "pi", "--root", t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := cases.Suite("pi")
	if opts.Suite != "pi" || !reflect.DeepEqual(opts.CaseIDs, want) {
		t.Fatalf("Pi suite options = %+v, want %v", opts, want)
	}
}

func TestParseReleaseArgsLabelsExplicitCases(t *testing.T) {
	want := []string{
		"claude-code-openviking-automatic-memory",
		"pi-openviking-automatic-memory",
	}
	opts, err := parseReleaseArgs(append([]string{"--root", t.TempDir()}, want...))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Suite != "custom" || !reflect.DeepEqual(opts.CaseIDs, want) {
		t.Fatalf("explicit case options = %+v, want suite=custom cases=%v", opts, want)
	}
}

func TestCandidateComparisonClassification(t *testing.T) {
	initial := releaseAttemptResult{
		Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("candidate failed")},
		FailedCases: []string{"case-a"},
	}
	passing := releaseAttemptResult{}
	tests := map[string]struct {
		base, retry releaseAttemptResult
		expect      releaseClassification
	}{
		"regression":  {passing, releaseAttemptResult{Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("failed again")}}, releaseClassificationProbableRegression},
		"baseline":    {releaseAttemptResult{Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("base failed")}, FailedCases: []string{"case-a"}}, releaseAttemptResult{}, releaseClassificationBaselineIncompatible},
		"environment": {passing, releaseAttemptResult{Class: releaseFailureEnvironment, Outcome: releasegate.Outcome{Cleanup: errors.New("cleanup failed")}}, releaseClassificationEnvironmentFailure},
		"flaky":       {passing, passing, releaseClassificationFlakyInconclusive},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := classifyCandidateComparison(initial, tc.base, tc.retry)
			if got.Classification != tc.expect || got.Outcome.Primary == nil || !strings.Contains(got.Outcome.Primary.Error(), string(tc.expect)) {
				t.Fatalf("classification = %q / %v, want %q", got.Classification, got.Outcome.Primary, tc.expect)
			}
		})
	}
}

func TestReleaseCaseFailureClassification(t *testing.T) {
	for _, status := range []string{runner.StatusJudgeFail, runner.StatusExecutionFail, runner.StatusTimedOut} {
		if got := releaseCaseFailureClass(status); got != releaseFailureEnvironment {
			t.Errorf("status %q classified as %q, want environment", status, got)
		}
	}
	for _, status := range []string{runner.StatusSemanticFail, runner.StatusDetermFail, runner.StatusSoftFail} {
		if got := releaseCaseFailureClass(status); got != releaseFailureCase {
			t.Errorf("status %q classified as %q, want candidate-sensitive case", status, got)
		}
	}
}

func TestCandidateComparisonPreservesRegressionWhenBaseFailsOnlySubset(t *testing.T) {
	initial := releaseAttemptResult{
		Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("candidate failed a and b")},
		FailedCases: []string{"case-a", "case-b"},
	}
	base := releaseAttemptResult{
		Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("base failed b")},
		FailedCases: []string{"case-b"},
	}
	retry := releaseAttemptResult{
		Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("candidate failed a again")},
		FailedCases: []string{"case-a"},
	}

	got := classifyCandidateComparison(initial, base, retry)
	if got.Classification != releaseClassificationProbableRegression {
		t.Fatalf("classification = %q, want %q", got.Classification, releaseClassificationProbableRegression)
	}
	if message := got.Outcome.Primary.Error(); !strings.Contains(message, "baseline-incompatible cases") || !strings.Contains(message, "probable-openviking-regression") {
		t.Fatalf("mixed comparison evidence missing from %q", message)
	}
	if diff := failedCaseDifference(initial.FailedCases, base.FailedCases); !reflect.DeepEqual(diff, []string{"case-a"}) {
		t.Fatalf("candidate-only failures = %v, want [case-a]", diff)
	}
}

func TestClassificationWithoutComparisonIsTyped(t *testing.T) {
	if got := classificationWithoutComparison(releaseAttemptResult{
		Class: releaseFailureCase, Outcome: releasegate.Outcome{Primary: errors.New("case failed")},
	}); got != releaseClassificationUncomparedCaseFailure {
		t.Fatalf("case classification = %q", got)
	}
	if got := classificationWithoutComparison(releaseAttemptResult{
		Class: releaseFailureEnvironment, Outcome: releasegate.Outcome{Cleanup: errors.New("cleanup failed")},
	}); got != releaseClassificationEnvironmentFailure {
		t.Fatalf("environment classification = %q", got)
	}
}

func TestRequiredHarnessPackagesAreSuiteScoped(t *testing.T) {
	if got := requiredHarnessPackages([]string{"openviking-service-baseline"}); len(got) != 0 {
		t.Fatalf("baseline packages = %v", got)
	}
	got := requiredHarnessPackages([]string{"claude-code-openviking-automatic-memory", "hermes-openviking-tools", "pi-openviking-tools"})
	want := []string{"claude-code", "hermes-agent", "pi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("packages = %v, want %v", got, want)
	}
}

func TestPreflightSessionSuffixIsStableAndAttemptScoped(t *testing.T) {
	a := preflightSessionSuffix("/tmp/run/runtime/candidate-1")
	if a == "" || a != preflightSessionSuffix("/tmp/run/runtime/candidate-1") {
		t.Fatalf("unstable preflight suffix %q", a)
	}
	if a == preflightSessionSuffix("/tmp/run/runtime/candidate-2") {
		t.Fatal("fresh attempts reused the same preflight session suffix")
	}
}

func TestSelectedOpenVikingPluginsAreSuiteScoped(t *testing.T) {
	hermes := selectedOpenVikingPlugins([]string{"hermes-openviking-tools"})
	if len(hermes) != 0 {
		t.Fatalf("Hermes-only suite depends on unrelated OpenViking plugins: %v", hermes)
	}
	selected := selectedOpenVikingPlugins([]string{"codex-openviking-mcp-tools", "pi-openviking-tools"})
	if len(selected) != 2 || selected["codex"] != "codex-memory-plugin" || selected["pi"] != "pi-coding-agent-extension" {
		t.Fatalf("selected plugins = %v", selected)
	}
}

func TestHealthVersionRejectsAbsentAndNonStringValues(t *testing.T) {
	if got := healthVersion(map[string]any{"version": nil, "service_version": 123}); got != "" {
		t.Fatalf("health version = %q", got)
	}
	if got := healthVersion(map[string]any{"service_version": " 1.2.3 "}); got != "1.2.3" {
		t.Fatalf("health version = %q", got)
	}
}

func TestWriteHermesTemplateUsesScopedNamedProvider(t *testing.T) {
	home, err := writeHermesTemplate(t.TempDir(), releasegate.ModelCredentials{
		LLMBaseURL: "https://ark.example/api/v3", LLMModel: "model-x",
		LLMProtocol: releasegate.ProtocolOpenAIResponses,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	model, _ := config["model"].(map[string]any)
	agent, _ := config["agent"].(map[string]any)
	platformToolsets, _ := config["platform_toolsets"].(map[string]any)
	knownPluginToolsets, _ := config["known_plugin_toolsets"].(map[string]any)
	providers, _ := config["custom_providers"].([]any)
	if model["provider"] != "custom:ovtest" || agent["reasoning_effort"] != "none" || len(providers) != 1 {
		t.Fatalf("Hermes model/agent/provider config = %v / %v / %v", model, agent, providers)
	}
	provider, _ := providers[0].(map[string]any)
	if provider["base_url"] != "https://ark.example/api/v3" ||
		provider["api_mode"] != "codex_responses" ||
		provider["key_env"] != "OV_TEST_HERMES_LLM_API_KEY" {
		t.Fatalf("Hermes custom provider = %v", provider)
	}
	if cli, ok := platformToolsets["cli"].([]any); !ok || len(cli) != 0 {
		t.Fatalf("Hermes default CLI platform toolsets = %#v", platformToolsets["cli"])
	}
	if cli, ok := knownPluginToolsets["cli"].([]any); !ok || len(cli) != 1 || cli[0] != "memory" {
		t.Fatalf("Hermes known CLI plugin toolsets = %#v", knownPluginToolsets["cli"])
	}
	if strings.Contains(string(raw), "api_key") {
		t.Fatalf("Hermes template persisted a credential field: %s", raw)
	}
}

func TestApplyEnvironmentRestoresPreviousState(t *testing.T) {
	t.Setenv("OVTEST_RELEASE_EXISTING", "before")
	_ = os.Unsetenv("OVTEST_RELEASE_NEW")
	restore := applyEnvironment(map[string]string{
		"OVTEST_RELEASE_EXISTING": "during",
		"OVTEST_RELEASE_NEW":      "temporary",
	})
	if os.Getenv("OVTEST_RELEASE_EXISTING") != "during" || os.Getenv("OVTEST_RELEASE_NEW") != "temporary" {
		t.Fatal("environment was not applied")
	}
	restore()
	if os.Getenv("OVTEST_RELEASE_EXISTING") != "before" {
		t.Fatal("existing value was not restored")
	}
	if _, ok := os.LookupEnv("OVTEST_RELEASE_NEW"); ok {
		t.Fatal("new value was not removed")
	}
}

func TestApplySanitizedHarnessEnvironmentScopesAndRestoresCredentials(t *testing.T) {
	t.Setenv("OPENVIKING_LLM_API_KEY", "ambient-model-secret")
	t.Setenv("UNRELATED_SESSION_TOKEN", "ambient-unrelated-secret")
	t.Setenv("OV_TEST_CODEX_OPENVIKING_API_KEY", "old-scoped-key")

	restore := applySanitizedHarnessEnvironment(map[string]string{
		"OV_TEST_CODEX_OPENVIKING_API_KEY": "run-scoped-key",
	})
	if _, ok := os.LookupEnv("OPENVIKING_LLM_API_KEY"); ok {
		t.Fatal("OpenViking provider credential leaked into machine-auth harness")
	}
	if _, ok := os.LookupEnv("UNRELATED_SESSION_TOKEN"); ok {
		t.Fatal("unrelated ambient token leaked into harness")
	}
	if got := os.Getenv("OV_TEST_CODEX_OPENVIKING_API_KEY"); got != "run-scoped-key" {
		t.Fatalf("scoped OpenViking key = %q", got)
	}

	restore()
	if got := os.Getenv("OPENVIKING_LLM_API_KEY"); got != "ambient-model-secret" {
		t.Fatalf("provider credential was not restored: %q", got)
	}
	if got := os.Getenv("UNRELATED_SESSION_TOKEN"); got != "ambient-unrelated-secret" {
		t.Fatalf("unrelated token was not restored: %q", got)
	}
	if got := os.Getenv("OV_TEST_CODEX_OPENVIKING_API_KEY"); got != "old-scoped-key" {
		t.Fatalf("scoped key was not restored: %q", got)
	}
}

func TestHarnessEnvironmentScopesJudgeCredentialsToOpenVikingCases(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_TIMEOUT", "")
	t.Setenv("OVTEST_JUDGE_RETRIES", "")
	credentials := releasegate.RuntimeCredentials{Model: releasegate.ModelCredentials{
		LLMAPIKey: "model-secret", LLMBaseURL: "https://llm.example/v1", LLMModel: "model-x",
	}}
	openviking := harnessEnvironment("openviking", credentials)
	if openviking["ARK_API_KEY"] != "model-secret" || openviking["ARK_MODEL"] != "model-x" {
		t.Fatalf("OpenViking case environment = %v", openviking)
	}
	if openviking["OVTEST_JUDGE_TIMEOUT"] != "180" || openviking["OVTEST_JUDGE_RETRIES"] != "1" {
		t.Fatalf("OpenViking release judge defaults = %v", openviking)
	}
	if _, ok := harnessEnvironment("codex", credentials)["ARK_API_KEY"]; ok {
		t.Fatal("judge credential leaked to machine-auth harness")
	}
}

func TestHarnessEnvironmentPreservesExplicitJudgeTransportOverrides(t *testing.T) {
	t.Setenv("OVTEST_JUDGE_TIMEOUT", "90")
	t.Setenv("OVTEST_JUDGE_RETRIES", "1")
	got := harnessEnvironment("openviking", releasegate.RuntimeCredentials{})
	if got["OVTEST_JUDGE_TIMEOUT"] != "90" || got["OVTEST_JUDGE_RETRIES"] != "1" {
		t.Fatalf("judge transport overrides = %v", got)
	}
}

func TestCleanupReleaseURIsDeletesExactLocalOwnership(t *testing.T) {
	ownership := releasegate.NewOwnershipRegistry("run-abc")
	if err := ownership.OwnGenerated("viking://user/runner/memories/memory-123.md"); err != nil {
		t.Fatal(err)
	}
	var got []string
	outcome := cleanupReleaseURIs(context.Background(), releasegate.TargetLocal, ownership, nil, func(_ context.Context, uri string) error {
		got = append(got, uri)
		return nil
	})
	if outcome.Err() != nil || !reflect.DeepEqual(got, []string{"viking://user/runner/memories/memory-123.md"}) {
		t.Fatalf("local cleanup outcome=%+v got=%v", outcome, got)
	}
}

func TestCleanupCaseClaimsRejectsForgedClaimBeforeDelete(t *testing.T) {
	called := false
	err := cleanupCaseClaims(context.Background(), releasegate.TargetLocal, "run-abc", []ops.CleanupClaim{{
		URI: "viking://user/runner/memories/forged.md", Kind: "memory",
	}}, func(context.Context, string) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("forged claim err=%v called=%v", err, called)
	}
}

func TestCleanupReleaseURIsDeletesExactExternalOwnership(t *testing.T) {
	ownership := releasegate.NewOwnershipRegistry("run-abc")
	if err := ownership.Own("viking://resources/ovtest-runs/run-abc"); err != nil {
		t.Fatal(err)
	}
	var got []string
	outcome := cleanupReleaseURIs(context.Background(), releasegate.TargetExternal, ownership, nil, func(_ context.Context, uri string) error {
		got = append(got, uri)
		return nil
	})
	if outcome.Err() != nil || !reflect.DeepEqual(got, []string{"viking://resources/ovtest-runs/run-abc"}) {
		t.Fatalf("external cleanup outcome=%+v got=%v", outcome, got)
	}
}

func TestCleanupNeedsRecursiveOnlyForDirectoryErrors(t *testing.T) {
	for _, message := range []string{
		"plugin error: directory not empty: /resources/run-abc",
		"cannot remove: Is a directory",
		"Cannot remove directory without --recursive",
		"rerun with use --recursive",
	} {
		if !cleanupNeedsRecursive(message) {
			t.Fatalf("cleanupNeedsRecursive(%q) = false", message)
		}
	}
	for _, message := range []string{
		"Not a directory (os error 20)",
		"connection refused",
		"permission denied",
	} {
		if cleanupNeedsRecursive(message) {
			t.Fatalf("cleanupNeedsRecursive(%q) = true", message)
		}
	}
}

func TestCleanupURIRequiresRecursive(t *testing.T) {
	tests := []struct {
		uri  string
		want bool
	}{
		{"viking://user/runner/sessions/cc-session-id", true},
		{"viking://user/runner/memories/preferences/project.md", false},
		{"viking://resources/run-id/fixture", false},
		{"not a URI", false},
	}
	for _, test := range tests {
		if got := cleanupURIRequiresRecursive(test.uri); got != test.want {
			t.Errorf("cleanupURIRequiresRecursive(%q) = %v, want %v", test.uri, got, test.want)
		}
	}
}

func TestWriteHermesTemplateContainsNoCredential(t *testing.T) {
	home, err := writeHermesTemplate(t.TempDir(), releasegate.ModelCredentials{
		LLMAPIKey: "never-write-this", LLMBaseURL: "https://llm.example/v1", LLMModel: "model-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "never-write-this") {
		t.Fatal("Hermes template persisted an API key")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("JSON-compatible YAML template: %v", err)
	}
}

func TestReleaseEvidenceAndRecordsRedactSecrets(t *testing.T) {
	dir := t.TempDir()
	secret := "release-secret-value"
	result := runner.Result{ID: "redaction-case", Detail: "error included " + secret}
	if err := writeCaseEvidence(dir, result, []string{secret}); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(dir, "results.jsonl")
	if err := appendReleaseRecord(records, result, []string{secret}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "redaction-case.json"), records} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "[REDACTED]") {
			t.Fatalf("release artifact %s was not redacted: %s", path, raw)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("release artifact mode = %v", info.Mode().Perm())
		}
	}
}

func TestWriteReleaseSummaryRecordsTypedClassificationAndRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run-summary.json")
	secret := "summary-secret"
	outcome := releasegate.Outcome{Primary: errors.New("candidate failed with " + secret)}
	if err := writeReleaseSummary(path, "run-summary-test", outcome, releaseClassificationBaselineIncompatible, []string{secret}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("summary leaked secret: %s", raw)
	}
	var summary struct {
		Status         string                `json:"status"`
		Classification releaseClassification `json:"classification"`
		PrimaryError   string                `json:"primary_error"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Status != "primary_failed" || summary.Classification != releaseClassificationBaselineIncompatible || !strings.Contains(summary.PrimaryError, "[REDACTED]") {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestWriteClaudeAuthSettingsCopiesOnlyAllowedEnvironment(t *testing.T) {
	source := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(source, []byte(`{
  "theme":"dark",
  "env":{
    "ANTHROPIC_AUTH_TOKEN":"machine-token",
    "ANTHROPIC_BASE_URL":"https://claude.example",
    "ANTHROPIC_MODEL":"claude-test",
    "UNRELATED_SECRET":"must-not-copy"
  },
  "plugins":{"unsafe":true}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CLAUDE_AUTH_SETTINGS", source)
	destination, model, err := writeClaudeAuthSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if model != "claude-test" {
		t.Fatalf("model = %q", model)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "machine-token") || strings.Contains(text, "UNRELATED_SECRET") || strings.Contains(text, "plugins") {
		t.Fatalf("isolated Claude settings = %s", text)
	}
}

func TestDefaultOpenCodeConfigTemplateUsesIsolatedMachineTemplate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"provider":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_OPENCODE_CONFIG_TEMPLATE", "")
	t.Setenv("XDG_CONFIG_HOME", root)
	if got := defaultOpenCodeConfigTemplate(); got != path {
		t.Fatalf("defaultOpenCodeConfigTemplate() = %q, want %q", got, path)
	}
	explicit := filepath.Join(t.TempDir(), "custom.jsonc")
	t.Setenv("OV_TEST_OPENCODE_CONFIG_TEMPLATE", explicit)
	if got := defaultOpenCodeConfigTemplate(); got != explicit {
		t.Fatalf("explicit template = %q, want %q", got, explicit)
	}
}

func TestNewReleaseRunIDIsSafeAndUnique(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC)
	a, err := newReleaseRunID(now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newReleaseRunID(now)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasPrefix(a, "run-20260713t123456z-") {
		t.Fatalf("run IDs = %q, %q", a, b)
	}
}
