package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
)

// White-box unit tests for the operator layer: no live subprocess (runOv/runCLI/
// writeUserConf are stubbed) and no LLM. They pin the load-bearing design rules —
// the HARD/SOFT gate split, structural (not score) readiness, and the in-op target
// resolution — that status classification depends on.

func stubOv(fn func(args []string, conf string, settle int) cliResult) func() {
	orig, origCtx := runOv, runOvContext
	runOv = fn
	runOvContext = func(_ context.Context, args []string, conf string, settle int) cliResult {
		return fn(args, conf, settle)
	}
	writeOrig := writeUserConf
	writeUserConf = func(string) (string, error) { return "/tmp/fake.conf", nil }
	return func() { runOv, runOvContext = orig, origCtx; writeUserConf = writeOrig }
}

func stubAPI(fn func(req apiRequest) cliResult) func() {
	orig := runAPI
	runAPI = fn
	return func() { runAPI = orig }
}

func okJSON(result any) cliResult {
	body, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return cliResult{ExitCode: 0, Stdout: string(body)}
}

func mustJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	return out
}

func findStdout(mems []map[string]any) string {
	body, _ := json.Marshal(map[string]any{"result": map[string]any{"memories": mems}})
	return string(body)
}

func newOp(factory dag.Factory, name string, oc map[string]any) interface {
	Process(map[string]any) (map[string]any, error)
} {
	return factory.New(name, oc)
}

// ── gate severity (the design's central knob) ───────────────────────────────--

func TestGateDefaultIsHard(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 1, Stderr: "boom"}
	})()
	op := newOp(OvAddMemory, "add", map[string]any{"content": "c"})
	_, err := op.Process(map[string]any{"user_key": "uk"})
	var gf *GateFail
	if !errors.As(err, &gf) {
		t.Fatalf("hard gate must raise *GateFail, got %v", err)
	}
}

func TestGateSoftRecordsInsteadOfRaising(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 1, Stderr: "boom"}
	})()
	op := newOp(OvAddMemory, "add", map[string]any{"content": "c", "gate": "soft"})
	out, err := op.Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("soft gate must not raise: %v", err)
	}
	if out["ok"] != nil {
		t.Errorf("soft output must be nil, got %v", out["ok"])
	}
	gf, _ := out["gate_failed"].(map[string]any)
	if gf["node"] != "add" || !contains(gf["detail"].(string), "boom") {
		t.Errorf("gate_failed not recorded: %v", out["gate_failed"])
	}
}

func TestObserveModeSoftensNonCritical(t *testing.T) {
	t.Setenv("OVTEST_GATE_MODE", "observe")
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 1, Stderr: "boom"}
	})()
	out, err := newOp(OvAddMemory, "add", map[string]any{"content": "c"}).Process(map[string]any{"user_key": "uk"})
	if err != nil || out["gate_failed"] == nil {
		t.Fatalf("observe mode must soften non-critical gates: out=%v err=%v", out, err)
	}
	// per-node hard wins over observe
	_, err = newOp(OvAddMemory, "add", map[string]any{"content": "c", "gate": "hard"}).Process(map[string]any{"user_key": "uk"})
	if err == nil {
		t.Error("per-node hard must win over observe mode")
	}
}

func TestCriticalAndConfigErrorsNeverSoften(t *testing.T) {
	t.Setenv("OVTEST_GATE_MODE", "observe")
	// critical op (create): even gate:soft + observe must raise
	defer stubOv(func([]string, string, int) cliResult { return cliResult{ExitCode: 1, Stderr: "denied"} })()
	t.Setenv("OV_TEST_ROOT_CONF", "/etc/hosts") // exists, so root-conf check passes
	_, err := newOp(OvCreateAccount, "create", map[string]any{
		"account": "a", "admin_user": "u", "gate": "soft"}).Process(nil)
	if err == nil {
		t.Error("GATE_CRITICAL op must never soften")
	}
	// config error (missing content) must fail loud even under observe/soft
	_, err = newOp(OvAddMemory, "add", map[string]any{"gate": "soft"}).Process(map[string]any{"user_key": "uk"})
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Errorf("missing required config must raise *ConfigError, got %v", err)
	}
}

// ── need / user-key resolution ──────────────────────────────────────────────--

func TestNeedAndUserKeyResolution(t *testing.T) {
	b := &base{name: "t", oc: map[string]any{"content": "hi"}}
	if v, err := b.need("content"); err != nil || v != "hi" {
		t.Errorf("need present: %v %v", v, err)
	}
	if _, err := b.need("missing"); err == nil {
		t.Error("need missing must error")
	}

	writeOrig := writeUserConf
	writeUserConf = func(k string) (string, error) { return "/conf/" + k, nil }
	defer func() { writeUserConf = writeOrig }()

	// wired key wins; otherwise use the preconfigured ovcli.conf.
	if c, _ := userConf("n", "wired"); c != "/conf/wired" {
		t.Errorf("wired key: %s", c)
	}
	dir := t.TempDir()
	userPath := filepath.Join(dir, "ovcli.conf")
	if err := os.WriteFile(userPath, []byte(`{"url":"http://ov.local","api_key":"user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)
	if c, _ := userConf("n", nil); c != userPath {
		t.Errorf("configured user conf: %s", c)
	}
	t.Setenv("OV_TEST_CONF_DIR", filepath.Join(dir, "missing"))
	if _, err := userConf("n", nil); err == nil {
		t.Error("missing ovcli.conf must be a ConfigError")
	}
}

func TestUserConfWriteFailureFailsLoud(t *testing.T) {
	writeOrig := writeUserConf
	defer func() { writeUserConf = writeOrig }()

	writeUserConf = func(string) (string, error) { return "", errors.New("disk full") }
	_, err := userConf("n", "wired")
	var ce *ConfigError
	if !errors.As(err, &ce) || !contains(err.Error(), "could not write user config") ||
		!contains(err.Error(), "disk full") {
		t.Fatalf("write failure must be a loud config error, got %v", err)
	}

	writeUserConf = func(string) (string, error) { return "", nil }
	_, err = userConf("n", "wired")
	if !errors.As(err, &ce) || !contains(err.Error(), "empty config path") {
		t.Fatalf("empty config path must be a loud config error, got %v", err)
	}
}

// ── poll contract ───────────────────────────────────────────────────────────--

func TestPollReturnsResultAndAttempts(t *testing.T) {
	b := &base{name: "t"}
	n := 0
	out, attempts, err := b.poll(func(last bool) (map[string]any, error) {
		n++
		if n == 3 {
			return map[string]any{"ok": true}, nil
		}
		return nil, nil
	}, 5)
	if err != nil || out["ok"] != true || attempts != 3 {
		t.Fatalf("poll: out=%v attempts=%d err=%v", out, attempts, err)
	}
}

func TestPollRaisesOnContractBreak(t *testing.T) {
	b := &base{name: "t"}
	var flags []bool
	_, _, err := b.poll(func(last bool) (map[string]any, error) {
		flags = append(flags, last)
		return nil, nil // never returns/raises -> contract violation
	}, 2)
	if err == nil || !contains(err.Error(), "exhausted") {
		t.Fatalf("want contract-break error, got %v", err)
	}
	if len(flags) != 3 || !flags[2] {
		t.Errorf("last flag not set on final attempt: %v", flags)
	}
}

// ── OvFind: structural readiness (NOT score) + forbid + expect_gone ─────────--

func TestFindStructuralReadinessRejectsHighScoreWrongURI(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/experiences/record", "score": 0.99, "abstract": "prefers Go over Python"}})}
	})()
	op := newOp(OvFind, "find", map[string]any{"query": "lang?", "expect_uri": "/preferences/"})
	_, err := op.Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "no memory matching uri~") {
		t.Fatalf("a high-scoring wrong-uri memory must be filtered out, got %v", err)
	}
}

func TestFindPassesOnStructuralMatchAndScopesURI(t *testing.T) {
	var seen []string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		seen = args
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/preferences/lang", "score": 0.4, "abstract": "prefers go over python"}})}
	})()
	op := newOp(OvFind, "find", map[string]any{
		"query": "q", "uri": "viking://user/memories", "expect_uri": "/preferences/", "expect": []string{"go over python"}})
	out, err := op.Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("structural match should pass: %v", err)
	}
	if rel, _ := out["relevant"].([]map[string]any); len(rel) != 1 || out["attempts"] != 1 {
		t.Errorf("relevant/attempts: %v %v", out["relevant"], out["attempts"])
	} else if rel[0]["source_node"] != "find" {
		t.Errorf("relevant memory should include source node, got %v", rel[0])
	}
	if len(seen) != 4 || seen[2] != "-u" || seen[3] != "viking://user/memories" {
		t.Errorf("uri scope argv wrong: %v", seen)
	}
}

func TestFindPassesOnContentMatchWithoutURIShape(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/direct/lang", "score": 0.4, "abstract": "prefers go over python"}})}
	})()
	op := newOp(OvFind, "find", map[string]any{
		"query": "q", "expect": []string{"go", "python"}})
	out, err := op.Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("content match should pass without a path-shape assumption: %v", err)
	}
	if rel, _ := out["relevant"].([]map[string]any); len(rel) != 1 {
		t.Errorf("relevant: %v", out["relevant"])
	}
}

func TestFindPassesOnResourceMatch(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{
			"memories": []any{},
			"resources": []any{map[string]any{
				"uri":          "viking://resources/ovtest-codex/cxmcp-a1b2.md",
				"context_type": "resource",
				"score":        0.4,
				"abstract":     "fixture marker cxres-a1b2 checksum aurora quartz",
			}},
		})
	})()
	op := newOp(OvFind, "find", map[string]any{
		"query": "q", "expect": []string{"cxres-a1b2", "aurora quartz"}})
	out, err := op.Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("resource match should pass: %v", err)
	}
	rel, _ := out["relevant"].([]map[string]any)
	if len(rel) != 1 || rel[0]["context_type"] != "resource" {
		t.Errorf("relevant: %v", out["relevant"])
	}
}

func TestFindForbidLeakGate(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/x", "score": 0.3, "abstract": "the user's badge number is Z-9921"}})}
	})()
	op := newOp(OvFind, "find", map[string]any{
		"query": "badge?", "min_results": 0, "forbid": []string{"z-9921"}})
	_, err := op.Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "forbidden token") {
		t.Fatalf("a leaked never-stated token must hard-fail, got %v", err)
	}
}

func TestFindMemorySchemaErrorFailsEvenWhenZeroResultsAllowed(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: `{"ok":true,"result":{"memories":{"uri":"not-an-array"}}}`}
	})()
	op := newOp(OvFind, "find", map[string]any{"query": "q", "min_results": 0})
	_, err := op.Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "result.memories is not an array") {
		t.Fatalf("malformed memories schema must gate-fail, got %v", err)
	}
}

func TestFindExpectGoneInvertsReadiness(t *testing.T) {
	// present -> ghost fail
	restore := stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/y", "score": 0.3, "abstract": "clearance is Delta-9"}})}
	})
	op := newOp(OvFind, "find", map[string]any{
		"query": "clearance?", "expect": []string{"delta-9"}, "expect_gone": true, "retry": 1})
	_, err := op.Process(map[string]any{"user_key": "uk"})
	restore()
	if err == nil || !contains(err.Error(), "ghost resurfacing") {
		t.Fatalf("still-present memory must ghost-fail, got %v", err)
	}
	// absent -> success
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/y", "score": 0.3, "abstract": "an unrelated memory"}})}
	})()
	out, err := newOp(OvFind, "find", map[string]any{
		"query": "clearance?", "expect": []string{"delta-9"}, "expect_gone": true}).Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("absent memory must satisfy expect_gone: %v", err)
	}
	if rel, _ := out["relevant"].([]map[string]any); len(rel) != 0 {
		t.Errorf("relevant should be empty: %v", out["relevant"])
	}
}

func TestFindExposesOKForTerminalEvidenceSummary(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{"memories": []any{map[string]any{
			"uri":      "viking://user/memories/preferences/food.md",
			"abstract": "mango sticky rice",
		}}})
	})()
	out, err := newOp(OvFind, "find", map[string]any{
		"query": "mango sticky rice", "expect": []string{"mango sticky rice"},
	}).Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("find should pass: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("find should expose ok=true for terminal evidence summary, out=%v", out)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 0 {
		t.Fatalf("find without cleanup must expose typed empty claims, got %#v", out[CleanupClaimsOutput])
	}
}

func TestFindProducesTypedCleanupClaimsOnlyForMarkedRelevantResults(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/runner/memories/owned", "abstract": "marker run-unique-123 preference"},
			{"uri": "viking://user/runner/memories/unrelated", "abstract": "different memory"},
		})}
	})()
	out, err := newOp(OvFind, "find", map[string]any{
		"query": "marker", "expect": []string{"run-unique-123"},
		"cleanup_kind": "memory", "cleanup_marker": "run-unique-123",
	}).Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || claims[0].URI != "viking://user/runner/memories/owned" {
		t.Fatalf("cleanup claims = %#v", out[CleanupClaimsOutput])
	}
}

func TestFindCleanupClaimRejectsNonCanonicalMemoryURI(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/legacy", "abstract": "marker run-unique-123 preference"},
		})}
	})()
	_, err := newOp(OvFind, "find", map[string]any{
		"query": "marker", "expect": []string{"run-unique-123"},
		"cleanup_kind": "memory", "cleanup_marker": "run-unique-123",
	}).Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "not an exact memory result URI") {
		t.Fatalf("noncanonical cleanup URI should fail, got %v", err)
	}
}

func TestFindProducesCleanupClaimForCanonicalPeerMemory(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/runner/peers/hermes/memories/entities/mem_123.md", "abstract": "marker peer-run-123 preference"},
		})}
	})()
	out, err := newOp(OvFind, "find", map[string]any{
		"query": "marker", "expect": []string{"peer-run-123"},
		"cleanup_kind": "memory", "cleanup_marker": "peer-run-123",
	}).Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || !strings.Contains(claims[0].URI, "/peers/hermes/memories/") {
		t.Fatalf("cleanup claims = %#v", out[CleanupClaimsOutput])
	}
}

func TestFindCleanupClaimRejectsMissingMarkerProof(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://resources/shared/file.md", "abstract": "matching content"},
		})}
	})()
	_, err := newOp(OvFind, "find", map[string]any{
		"query": "matching", "expect": []string{"matching"},
		"cleanup_kind": "resource", "cleanup_marker": "run-unique-123",
	}).Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "lacks resource scope marker") {
		t.Fatalf("missing cleanup marker proof should fail, got %v", err)
	}
}

func TestFindProducesResourceClaimFromRunScopedURI(t *testing.T) {
	const root = "viking://resources/ovtest-runs/run-123/harness"
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": root + "/fixture/file.md", "abstract": "static fixture content"},
		})}
	})()
	out, err := newOp(OvFind, "find", map[string]any{
		"query": "fixture", "expect": []string{"static fixture"},
		"cleanup_kind": "resource", "cleanup_marker": root,
	}).Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || claims[0].URI != root+"/fixture/file.md" {
		t.Fatalf("cleanup claims = %#v", out[CleanupClaimsOutput])
	}
}

func TestFindRetriesUntilIndexed(t *testing.T) {
	n := 0
	defer stubOv(func([]string, string, int) cliResult {
		n++
		if n == 1 {
			return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{})}
		}
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/preferences/x", "score": 0.3, "abstract": "go"}})}
	})()
	op := newOp(OvFind, "find", map[string]any{"query": "q", "expect_uri": "/preferences/", "retry": 2})
	out, err := op.Process(map[string]any{"user_key": "uk"})
	if err != nil || out["attempts"] != 2 {
		t.Fatalf("async readiness poll: attempts=%v err=%v", out["attempts"], err)
	}
}

func TestFindFailureIncludesSampleMemories(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{
			{"uri": "viking://user/memories/.abstract.md", "score": 0.3, "abstract": "generic memory directory"}})}
	})()
	op := newOp(OvFind, "find", map[string]any{
		"query": "q", "expect": []string{"go", "python"}})
	_, err := op.Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "sample: viking://user/memories/.abstract.md => generic memory directory") {
		t.Fatalf("failure should include returned-memory sample, got %v", err)
	}
}

func TestURIAbsentPollsTheExactURIUntilNotFound(t *testing.T) {
	var calls int
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		calls++
		if len(args) != 2 || args[0] != "read" || args[1] != "viking://user/memories/exact.md" {
			t.Fatalf("unexpected argv: %v", args)
		}
		if calls == 1 {
			return okJSON("still present")
		}
		return cliResult{ExitCode: 1, Stderr: "NOT_FOUND: File not found: exact.md"}
	})()
	out, err := newOp(OvURIAbsent, "absent", map[string]any{"settle": 0, "retry": 2}).Process(map[string]any{
		"user_key": "uk", "uri": "viking://user/memories/exact.md",
	})
	if err != nil || out["ok"] != true || out["attempts"] != 2 {
		t.Fatalf("exact absence result=%v err=%v", out, err)
	}
}

func TestURIAbsentRejectsNonNotFoundErrors(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 1, Stderr: "authentication failed"}
	})()
	_, err := newOp(OvURIAbsent, "absent", map[string]any{"settle": 0}).Process(map[string]any{
		"user_key": "uk", "uri": "viking://user/memories/exact.md",
	})
	if err == nil || !contains(err.Error(), "authentication failed") {
		t.Fatalf("non-NOT_FOUND error must fail, got %v", err)
	}
}

func TestURIAbsentFailsWhenExactURIStaysReadable(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult { return okJSON("still present") })()
	_, err := newOp(OvURIAbsent, "absent", map[string]any{"settle": 0, "retry": 1}).Process(map[string]any{
		"user_key": "uk", "uri": "viking://user/memories/exact.md",
	})
	if err == nil || !contains(err.Error(), "still readable after 2 attempts") {
		t.Fatalf("readable URI must fail, got %v", err)
	}
}

// ── OvRm: in-op target resolution ───────────────────────────────────────────--

func TestRmResolvesByAbstractFilter(t *testing.T) {
	var seen []string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		seen = args
		return cliResult{ExitCode: 0, Stdout: `{"ok":true,"result":{"removed":true}}`}
	})()
	mems := []map[string]any{
		{"uri": "viking://user/memories/a", "abstract": "unrelated"},
		{"uri": "viking://user/memories/b", "abstract": "clearance is Delta-9"}}
	out, err := newOp(OvRemove, "rm", map[string]any{"abstract_filter": "delta-9"}).Process(
		map[string]any{"user_key": "uk", "memories": mems})
	if err != nil || out["removed_uri"] != "viking://user/memories/b" {
		t.Fatalf("rm by abstract: %v %v", out["removed_uri"], err)
	}
	if len(seen) != 2 || seen[1] != "viking://user/memories/b" {
		t.Errorf("rm argv: %v", seen)
	}
}

func TestRmAllMatchesRemovesEveryMatchedURI(t *testing.T) {
	var removed []string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		removed = append(removed, args[1])
		return cliResult{ExitCode: 0, Stdout: `{"ok":true,"result":{"removed":true}}`}
	})()
	mems := []map[string]any{
		{"uri": "viking://user/memories/profile", "abstract": "clearance is Delta-9"},
		{"uri": "viking://user/memories/trajectory", "abstract": "clearance is Delta-9"},
		{"uri": "viking://user/memories/trajectory", "abstract": "clearance is Delta-9 again"},
		{"uri": "viking://user/memories/other", "abstract": "unrelated"},
	}
	out, err := newOp(OvRemove, "rm", map[string]any{
		"abstract_filter": "delta-9", "all_matches": true,
	}).Process(map[string]any{"user_key": "uk", "memories": mems})
	if err != nil {
		t.Fatalf("rm all matches: %v", err)
	}
	want := []string{"viking://user/memories/profile", "viking://user/memories/trajectory"}
	if !equalStrs(removed, want) {
		t.Fatalf("removed = %v", removed)
	}
	got := out["removed_uris"].([]string)
	if !equalStrs(got, want) {
		t.Fatalf("removed_uris = %v", got)
	}
}

func TestRmRaisesWhenNoMatch(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult { return cliResult{ExitCode: 0, Stdout: "{}"} })()
	_, err := newOp(OvRemove, "rm", map[string]any{"abstract_filter": "nope"}).Process(
		map[string]any{"user_key": "uk", "memories": []map[string]any{{"uri": "u", "abstract": "x"}}})
	if err == nil || !contains(err.Error(), "no uri to remove") {
		t.Fatalf("want no-match error, got %v", err)
	}
}

// ── session ops: ordering gate + two-phase commit ───────────────────────────--

func TestSessionAddMessageOrderingGate(t *testing.T) {
	var seen []string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		seen = args
		return okJSON(map[string]any{"session_id": "sid-1", "message_count": 2})
	})()
	out, err := newOp(OvSessionAddMessage, "msg_2", map[string]any{
		"role": "assistant", "content": "noted", "expect_count": 2}).Process(
		map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil || out["ok"] != true {
		t.Fatalf("matching position should pass: %v %v", out, err)
	}
	want := []string{"session", "add-message", "sid-1", "--role", "assistant", "--content", "noted"}
	if !equalStrs(seen, want) {
		t.Errorf("argv = %v", seen)
	}
	// mismatch -> gate fail
	_, err = newOp(OvSessionAddMessage, "msg_3", map[string]any{
		"content": "x", "expect_count": 3}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err == nil || !contains(err.Error(), "transcript position") {
		t.Fatalf("position mismatch must gate-fail, got %v", err)
	}
}

func TestSessionCommitTwoPhaseGate(t *testing.T) {
	taskPolls := 0
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		if len(args) >= 2 && args[0] == "session" && args[1] == "commit" {
			return okJSON(map[string]any{"session_id": "sid-1", "status": "accepted",
				"task_id": "t-1", "archive_uri": "viking://.../archive_001", "archived": true})
		}
		taskPolls++
		if taskPolls == 1 {
			return okJSON(map[string]any{"task_id": "t-1", "status": "running"})
		}
		return okJSON(map[string]any{"task_id": "t-1", "status": "completed", "result": map[string]any{
			"memories_extracted": map[string]any{"preferences": 3, "entities": 2},
		}})
	})()
	out, err := newOp(OvSessionCommit, "commit", map[string]any{"settle": 0, "retry": 3}).Process(
		map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil || out["ok"] != true || out["attempts"] != 2 || out["task_id"] != "t-1" {
		t.Fatalf("two-phase commit: out=%v err=%v", out, err)
	}
	ex, _ := out["extracted"].(map[string]any)
	if asInt(ex["total"], 0) != 5 {
		t.Errorf("extracted total: %v", out["extracted"])
	}
	if claims, ok := out[CleanupClaimsOutput].([]CleanupClaim); !ok || len(claims) != 0 {
		t.Fatalf("cleanup claims = %#v, want typed empty slice", out[CleanupClaimsOutput])
	}
}

func TestSessionCommitClaimsOnlyExactAddedMemoriesFromDiff(t *testing.T) {
	diff := `{"operations":{"adds":[{"uri":"viking://user/runner/memories/preferences/new.md"}],"updates":[{"uri":"viking://user/runner/memories/preferences/existing.md"}],"deletes":[]}}`
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		switch {
		case len(args) >= 2 && args[0] == "session" && args[1] == "commit":
			return okJSON(map[string]any{"status": "accepted", "task_id": "t-1", "archived": true})
		case len(args) >= 2 && args[0] == "task" && args[1] == "status":
			return okJSON(map[string]any{"task_id": "t-1", "status": "completed", "result": map[string]any{
				"memories_extracted": map[string]any{"memory_write": 1},
				"memory_diff_uri":    "viking://user/runner/sessions/sid-1/history/archive_001/memory_diff.json",
			}})
		case len(args) == 2 && args[0] == "read":
			return okJSON(diff)
		default:
			t.Fatalf("unexpected ov args: %v", args)
			return cliResult{ExitCode: 1}
		}
	})()
	out, err := newOp(OvSessionCommit, "commit", map[string]any{
		"settle": 0, "retry": 0, "cleanup_added_memories": true,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || claims[0].URI != "viking://user/runner/memories/preferences/new.md" {
		t.Fatalf("cleanup claims = %#v", out[CleanupClaimsOutput])
	}
}

func TestSessionCommitCleanupClaimsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		read    cliResult
		want    string
	}{
		{
			name: "missing diff URI",
			payload: map[string]any{
				"memories_extracted": map[string]any{"memory_write": 1},
			},
			want: "omitted memory_diff_uri",
		},
		{
			name: "namespace root add",
			payload: map[string]any{
				"memories_extracted": map[string]any{"memory_write": 1},
				"memory_diff_uri":    "viking://user/runner/sessions/sid-1/history/archive_001/memory_diff.json",
			},
			read: okJSON(`{"operations":{"adds":[{"uri":"viking://user/runner/memories"}]}}`),
			want: "not an exact memory URI",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer stubOv(func(args []string, _ string, _ int) cliResult {
				if len(args) >= 2 && args[0] == "session" && args[1] == "commit" {
					return okJSON(map[string]any{"status": "accepted", "task_id": "t-1", "archived": true})
				}
				if len(args) >= 2 && args[0] == "task" && args[1] == "status" {
					return okJSON(map[string]any{"task_id": "t-1", "status": "completed", "result": tc.payload})
				}
				return tc.read
			})()
			_, err := newOp(OvSessionCommit, "commit", map[string]any{
				"settle": 0, "retry": 0, "cleanup_added_memories": true,
			}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSessionCommitFailsWhenExtractionNeverReady(t *testing.T) {
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		if len(args) >= 2 && args[1] == "commit" {
			return okJSON(map[string]any{"status": "accepted", "task_id": "t-1", "archived": true})
		}
		return okJSON(map[string]any{"task_id": "t-1", "status": "completed", "result": map[string]any{
			"memories_extracted": map[string]any{},
		}})
	})()
	_, err := newOp(OvSessionCommit, "commit", map[string]any{"settle": 0, "retry": 2}).Process(
		map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err == nil || !contains(err.Error(), "completed with 0 extracted memories") {
		t.Fatalf("zero-extraction must localise at commit, got %v", err)
	}
}

func TestSessionCommitSurfacesFailedTask(t *testing.T) {
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		if len(args) >= 2 && args[1] == "commit" {
			return okJSON(map[string]any{"status": "accepted", "task_id": "t-1", "archived": true})
		}
		return okJSON(map[string]any{"task_id": "t-1", "status": "failed", "error": "provider timeout"})
	})()
	_, err := newOp(OvSessionCommit, "commit", map[string]any{"settle": 0, "retry": 2}).Process(
		map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err == nil || !contains(err.Error(), "commit task failed: provider timeout") {
		t.Fatalf("failed task must fail immediately with its error, got %v", err)
	}
}

func TestSessionCommitSurfacesMalformedTaskStatusJSON(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   string
	}{
		{"bad-json", "not json", "commit task status output not JSON"},
		{"bad-result-schema", `{"ok":true,"result":[1]}`, "commit task status result is not an object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer stubOv(func(args []string, _ string, _ int) cliResult {
				if len(args) >= 2 && args[1] == "commit" {
					return okJSON(map[string]any{"status": "accepted", "task_id": "t-1", "archived": true})
				}
				return cliResult{ExitCode: 0, Stdout: tc.stdout}
			})()
			_, err := newOp(OvSessionCommit, "commit", map[string]any{"settle": 0, "retry": 0}).Process(
				map[string]any{"user_key": "uk", "session_id": "sid-1"})
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("malformed task status response must gate-fail with schema evidence, got %v", err)
			}
		})
	}
}

// ── session_new + search gates ──────────────────────────────────────────────--

func TestSessionNewGate(t *testing.T) {
	restore := stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{"session_id": "sid-1", "uri": "viking://user/u/sessions/sid-1"})
	})
	out, err := newOp(OvSessionNew, "session_new", nil).Process(map[string]any{"user_key": "uk"})
	restore()
	if err != nil || out["session_id"] != "sid-1" {
		t.Fatalf("session new: %v %v", out, err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || claims[0].URI != "viking://user/u/sessions/sid-1" {
		t.Fatalf("session cleanup claim = %#v", out[CleanupClaimsOutput])
	}
	defer stubOv(func([]string, string, int) cliResult { return okJSON(map[string]any{}) })()
	_, err = newOp(OvSessionNew, "session_new", nil).Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "no session_id") {
		t.Fatalf("missing session_id must gate-fail, got %v", err)
	}
}

func TestSessionCommittedClaimsServerConfirmedCanonicalURI(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{
			"uri": "viking://user/u/sessions/sid-1", "commit_count": 1,
			"memories_extracted": map[string]any{"total": 1},
		})
	})()
	out, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 0, "retry": 0,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || claims[0].URI != "viking://user/u/sessions/sid-1" {
		t.Fatalf("session cleanup claim = %#v", out[CleanupClaimsOutput])
	}
}

func TestSessionPresentClaimsOnlyServerConfirmedSession(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{"uri": "viking://user/u/sessions/sid-1", "session_id": "sid-1"})
	})()
	out, err := newOp(OvSessionPresent, "present", nil).Process(map[string]any{
		"user_key": "uk", "session_id": "sid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 1 || claims[0].URI != "viking://user/u/sessions/sid-1" {
		t.Fatalf("session cleanup claim = %#v", out[CleanupClaimsOutput])
	}
}

func TestSearchMinResultsGate(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: findStdout([]map[string]any{})}
	})()
	_, err := newOp(OvSearch, "search", map[string]any{"query": "q", "min_results": 1}).Process(
		map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "search returned 0 memories") {
		t.Fatalf("search below min_results must gate-fail, got %v", err)
	}
}

func TestSearchMemorySchemaErrorFailsEvenWhenZeroResultsAllowed(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 0, Stdout: `{"ok":true,"result":{"memories":[42]}}`}
	})()
	_, err := newOp(OvSearch, "search", map[string]any{"query": "q", "min_results": 0}).Process(
		map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "result.memories[0] is not an object") {
		t.Fatalf("malformed search memories schema must gate-fail, got %v", err)
	}
}

func TestSessionNewWithMemoryPolicyUsesHTTPAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ovcli.conf"), []byte(`{"url":"http://ov.local","api_key":"user-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)

	var seen apiRequest
	defer stubAPI(func(req apiRequest) cliResult {
		seen = req
		return cliResult{ExitCode: 0, Stdout: `{"status":"ok","result":{"session_id":"sid-1","uri":"viking://user/default/sessions/sid-1"}}`}
	})()

	out, err := newOp(OvSessionNew, "session_new", map[string]any{
		"memory_policy": map[string]any{"memory_types": []any{"trajectories", "experiences"}},
	}).Process(map[string]any{"user_key": ""})
	if err != nil {
		t.Fatalf("session new with memory policy should pass: %v", err)
	}
	if out["session_id"] != "sid-1" {
		t.Fatalf("session_id = %v", out["session_id"])
	}
	if seen.Method != "POST" || seen.Path != "/api/v1/sessions" {
		t.Fatalf("request target = %s %s", seen.Method, seen.Path)
	}
	if seen.Headers["X-API-Key"] != "user-key" {
		t.Fatalf("X-API-Key header = %q", seen.Headers["X-API-Key"])
	}
	body := map[string]any{}
	if err := json.Unmarshal(seen.Body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	policy, _ := body["memory_policy"].(map[string]any)
	if got := asStrings(policy["memory_types"]); !equalStrs(got, []string{"trajectories", "experiences"}) {
		t.Fatalf("memory_types = %v", got)
	}
}

func TestSessionAddMessagesUsesHTTPBatchAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ovcli.conf"), []byte(`{"url":"http://ov.local","api_key":"user-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)

	messages := []map[string]any{
		{
			"role": "assistant",
			"parts": []map[string]any{
				{"type": "text", "text": "I will search first."},
				{"type": "tool", "tool_id": "call_1", "tool_name": "project_search",
					"tool_input": map[string]any{"query": "Apollo"}, "tool_output": `{"results":[]}`, "tool_status": "completed"},
			},
		},
	}

	var seen apiRequest
	defer stubAPI(func(req apiRequest) cliResult {
		seen = req
		return cliResult{ExitCode: 0, Stdout: `{"status":"ok","result":{"session_id":"sid-1","message_count":1,"added":1}}`}
	})()

	out, err := newOp(OvSessionAddMessages, "seed_transcript", nil).Process(map[string]any{
		"user_key": "", "session_id": "sid-1", "messages": messages,
	})
	if err != nil {
		t.Fatalf("batch add messages should pass: %v", err)
	}
	if out["ok"] != true || out["added"] != float64(1) || out["message_count"] != float64(1) {
		t.Fatalf("out = %v", out)
	}
	if seen.Method != "POST" || seen.Path != "/api/v1/sessions/sid-1/messages/batch" {
		t.Fatalf("request target = %s %s", seen.Method, seen.Path)
	}
	body := map[string]any{}
	if err := json.Unmarshal(seen.Body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if got := resultCount(body["messages"]); got != 1 {
		t.Fatalf("posted message count = %d", got)
	}
}

func TestSessionAddMessagesAcceptsDAGListShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ovcli.conf"), []byte(`{"url":"http://ov.local","api_key":"user-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OV_TEST_CONF_DIR", dir)

	var seen apiRequest
	defer stubAPI(func(req apiRequest) cliResult {
		seen = req
		return cliResult{ExitCode: 0, Stdout: `{"status":"ok","result":{"message_count":1,"added":1}}`}
	})()

	_, err := newOp(OvSessionAddMessages, "seed_transcript", nil).Process(map[string]any{
		"user_key": "", "session_id": "sid-1",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatalf("batch add messages should accept []any DAG shape: %v", err)
	}
	if got := resultCount(mustJSONMap(t, seen.Body)["messages"]); got != 1 {
		t.Fatalf("posted message count = %d", got)
	}
}

// ── OvWait: SOFT, never raises ──────────────────────────────────────────────--

func TestWaitSoftNeverRaises(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return cliResult{ExitCode: 1, Stdout: "not json <<<", Stderr: "boom"}
	})()
	out, err := newOp(OvWait, "wait", map[string]any{}).Process(map[string]any{"user_key": "uk"})
	if err != nil {
		t.Fatalf("ov_wait must never raise: %v", err)
	}
	if out["ok"] != false || out["processed"] != nil {
		t.Errorf("wait soft result: %v", out)
	}
}

// ── memories parse surfaces errors ──────────────────────────────────────────--

func TestMemoriesParse(t *testing.T) {
	mems, perr := memoriesOf("not json at all")
	if perr == "" || len(mems) != 0 {
		t.Errorf("parse error must surface: %v %q", mems, perr)
	}
	mems, perr = memoriesOf(findStdout([]map[string]any{{"uri": "u1", "score": 0.9, "abstract": "a"}}))
	if perr != "" || len(mems) != 1 || mems[0]["uri"] != "u1" {
		t.Errorf("clean parse: %v %q", mems, perr)
	}
	// a valid-but-non-object top level (bare array/scalar) is a schema error,
	// matching Python's (parsed or {}).get on a truthy non-dict; a falsy one isn't
	if _, perr = memoriesOf("[1,2,3]"); perr == "" {
		t.Error("truthy non-object top-level must surface a parse error")
	}
	if _, err := resultOf("[]"); err != nil {
		t.Errorf("falsy non-object ([]) must not error: %v", err)
	}
	for name, body := range map[string]string{
		"missing":    `{"ok":true,"result":{}}`,
		"non-array":  `{"ok":true,"result":{"memories":{"uri":"u"}}}`,
		"non-object": `{"ok":true,"result":{"memories":[42]}}`,
	} {
		if _, perr = memoriesOf(body); perr == "" {
			t.Errorf("%s memories schema error must surface", name)
		}
	}
}

func TestAsMemListFlattensMergedFindOutputs(t *testing.T) {
	merged := []any{
		[]map[string]any{{"uri": "viking://user/memories/experiences/a", "abstract": "first"}},
		[]map[string]any{{"uri": "viking://user/memories/trajectories/b", "abstract": "second"}},
	}
	mems := asMemList(merged)
	if len(mems) != 2 {
		t.Fatalf("merged memory lists should flatten to two memories, got %#v", mems)
	}
	if mems[0]["uri"] != "viking://user/memories/experiences/a" ||
		mems[1]["uri"] != "viking://user/memories/trajectories/b" {
		t.Fatalf("flattened memories in wrong order: %#v", mems)
	}
}

// ── admin --sudo passthrough ────────────────────────────────────────────────--

func TestAdminSudoPassthrough(t *testing.T) {
	t.Setenv("OV_TEST_ROOT_CONF", "/etc/hosts") // exists
	var seen []string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		seen = args
		return okJSON(map[string]any{"user_key": "k", "account_id": "a"})
	})()
	t.Setenv("OV_TEST_SUDO", "")
	newOp(OvCreateAccount, "create", map[string]any{"account": "a", "admin_user": "u"}).Process(nil)
	if hasStr(seen, "--sudo") {
		t.Errorf("no --sudo without OV_TEST_SUDO: %v", seen)
	}
	t.Setenv("OV_TEST_SUDO", "1")
	newOp(OvCreateAccount, "create", map[string]any{"account": "a", "admin_user": "u"}).Process(nil)
	want := []string{"admin", "create-account", "--sudo", "--admin", "u", "a"}
	if !equalStrs(seen, want) {
		t.Errorf("sudo argv = %v", seen)
	}
}

// ── generic CLI command + deterministic terminal check ──────────────────────--

func TestCommandRunsAdminAndChecksOutput(t *testing.T) {
	t.Setenv("OV_TEST_ROOT_CONF", "/etc/hosts") // exists
	var seen []string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		seen = args
		return okJSON(map[string]any{"task_id": "t-1", "status": "completed"})
	})()
	out, err := newOp(OvCommand, "reindex", map[string]any{
		"admin": true,
		"args":  []string{"reindex", "viking://x", "--mode", "semantic_and_vectors", "--wait", "true"},
		"expect": []string{
			"task_id",
			"completed",
		},
	}).Process(map[string]any{"user_key": "uk"})
	if err != nil || out["ok"] != true {
		t.Fatalf("admin command should pass: out=%v err=%v", out, err)
	}
	want := []string{"reindex", "viking://x", "--mode", "semantic_and_vectors", "--wait", "true"}
	if !equalStrs(seen, want) {
		t.Errorf("argv = %v", seen)
	}
}

func TestCommandForbidGate(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{"matches": []any{map[string]any{"uri": "viking://x/secret.tmp"}}})
	})()
	_, err := newOp(OvCommand, "grep", map[string]any{
		"args":   []string{"grep", "secret", "-u", "viking://x"},
		"forbid": []string{"secret.tmp"},
	}).Process(map[string]any{"user_key": "uk"})
	if err == nil || !contains(err.Error(), "forbidden token") {
		t.Fatalf("forbid token must gate-fail, got %v", err)
	}
}

func TestCommandCountsResourceFindResults(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{"resources": []any{map[string]any{"uri": "viking://x/a.md", "abstract": "violet quantum"}}})
	})()
	out, err := newOp(OvCommand, "find", map[string]any{
		"args":      []string{"find", "violet quantum", "-u", "viking://x"},
		"expect":    []string{"violet quantum"},
		"min_count": 1,
	}).Process(map[string]any{"user_key": "uk"})
	if err != nil || out["count"] != 1 {
		t.Fatalf("resource results should count: out=%v err=%v", out, err)
	}
}

func TestCheckTerminalVerdict(t *testing.T) {
	out, err := newOp(OvCheck, "check", map[string]any{"explanation": "all good"}).Process(
		map[string]any{"after": []any{true, "ok"}})
	if err != nil {
		t.Fatalf("truthy inputs should pass: %v", err)
	}
	verdict := out["verdict"].(map[string]any)
	if verdict["pass"] != true || verdict["explanation"] != "all good" {
		t.Fatalf("verdict = %v", verdict)
	}
	_, err = newOp(OvCheck, "check", nil).Process(map[string]any{"after": []any{true, false}})
	if err == nil || !contains(err.Error(), "deterministic check input was false") {
		t.Fatalf("false input must fail deterministically, got %v", err)
	}
}

func TestTextCheckRequiresExpectedTokens(t *testing.T) {
	out, err := newOp(TextCheck, "reply_check", map[string]any{
		"expect": []string{"PX-123", "cobalt teal"},
	}).Process(map[string]any{"text": "The answer is PX-123 and cobalt teal."})
	if err != nil {
		t.Fatalf("text check should pass: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("out = %v", out)
	}

	_, err = newOp(TextCheck, "reply_check", map[string]any{
		"expect": []string{"PX-123", "cobalt teal"},
	}).Process(map[string]any{"text": "PX-123"})
	var gf *GateFail
	if !errors.As(err, &gf) || !contains(gf.Detail, "cobalt teal") {
		t.Fatalf("missing expected token must be gate fail with token detail, got %v", err)
	}
}

func TestTextCheckRejectsForbiddenTokens(t *testing.T) {
	_, err := newOp(TextCheck, "reply_check", map[string]any{
		"forbid": []string{"I do not know"},
	}).Process(map[string]any{"text": "I do not know the answer."})
	var gf *GateFail
	if !errors.As(err, &gf) || !contains(gf.Detail, "i do not know") {
		t.Fatalf("forbidden token must be gate fail with token detail, got %v", err)
	}
}

func TestTextCheckAcceptsAnyEquivalentToken(t *testing.T) {
	for _, text := range []string{
		"Basalt launches March 14, 2027.",
		"Basalt launches 2027-03-14.",
	} {
		_, err := newOp(TextCheck, "reply_check", map[string]any{
			"expect":     []string{"basalt", "2027"},
			"expect_any": []string{"march 14", "03-14"},
		}).Process(map[string]any{"text": text})
		if err != nil {
			t.Fatalf("equivalent date %q should pass: %v", text, err)
		}
	}
	_, err := newOp(TextCheck, "reply_check", map[string]any{
		"expect_any": []string{"march 14", "03-14"},
	}).Process(map[string]any{"text": "The launch is in 2027."})
	if err == nil || !contains(err.Error(), "accepted alternative") {
		t.Fatalf("missing all alternatives must fail, got %v", err)
	}
}

// ── tiny helpers ────────────────────────────────────────────────────────────--

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return s == sub
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

func hasStr(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
