package ops

import (
	"errors"
	"strings"
	"testing"
)

// The judge renders EXACTLY the non-nil wired evidence (in EVIDENCE order) and
// skips un-wired inputs — so the prompt shows only the evidence the case chose to
// score. Each memory is rendered with its uri (so a reference may distinguish
// /preferences/ from /trajectories/).

func TestJudgeRendersOnlyWiredEvidence(t *testing.T) {
	var captured string
	defer stubArk(func(_, user string) (string, error) {
		captured = user
		return "VERDICT: PASS\nok", nil
	})()
	_, err := newOp(OvJudge, "judge", map[string]any{"reference": "ref"}).Process(
		map[string]any{"reply": "the codename is Basalt", "transcript": "user: Basalt March 14"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(captured, "openviking session transcript:") || !contains(captured, "openclaw recall reply:") {
		t.Errorf("wired evidence missing:\n%s", captured)
	}
	if contains(captured, "find memories:") || contains(captured, "ls entries:") {
		t.Errorf("un-wired evidence must not appear:\n%s", captured)
	}
	if strings.Index(captured, "session transcript") > strings.Index(captured, "recall reply") {
		t.Error("EVIDENCE order: transcript must precede reply")
	}
}

func TestJudgeRendersMemoriesWithURIAndVerdictIsData(t *testing.T) {
	var captured string
	defer stubArk(func(_, user string) (string, error) {
		captured = user
		return "VERDICT: FAIL\ndid not meet criteria", nil
	})()
	out, err := newOp(OvJudge, "judge", map[string]any{"reference": "ref"}).Process(map[string]any{
		"memories": []map[string]any{{
			"source_node": "find_experiences_structured_tool_error_recovery",
			"uri":         "viking://user/memories/preferences/lang",
			"abstract":    "go",
			"score":       0.4,
		}},
		"search_memories": []map[string]any{{"uri": "u2", "abstract": "rust", "score": 0.3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(captured, "viking://user/memories/preferences/lang") {
		t.Errorf("memory uri must be rendered:\n%s", captured)
	}
	if !contains(captured, "find_experiences_structured_tool_error_recovery") {
		t.Errorf("memory source node must be rendered:\n%s", captured)
	}
	if !contains(captured, "find memories:") || !contains(captured, "search memories:") {
		t.Errorf("find and search render as distinct blocks:\n%s", captured)
	}
	// a FAIL verdict is DATA (no error), surfaced for the runner to map to semantic_failed
	if v, _ := out["verdict"].(map[string]any); v["pass"] != false {
		t.Errorf("FAIL verdict must be returned as data, got %v", out["verdict"])
	}
}

func TestJudgeBalancesLargeMemoriesAcrossEvidenceSources(t *testing.T) {
	var captured string
	defer stubArk(func(_, user string) (string, error) {
		captured = user
		return "VERDICT: PASS\nok", nil
	})()

	large := strings.Repeat("x", 4000)
	memories := []map[string]any{
		{"source_node": "scenario_a", "uri": "a-1", "abstract": "lesson-a " + large},
		{"source_node": "scenario_a", "uri": "a-2", "abstract": "extra-a " + large},
		{"source_node": "scenario_b", "uri": "b-1", "abstract": "lesson-b " + large},
		{"source_node": "scenario_c", "uri": "c-1", "abstract": "lesson-c " + large},
	}
	if _, err := newOp(OvJudge, "judge", map[string]any{"reference": "ref"}).Process(
		map[string]any{"memories": memories}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"scenario_a", "lesson-a", "scenario_b", "lesson-b", "scenario_c", "lesson-c"} {
		if !contains(captured, want) {
			t.Fatalf("balanced judge evidence missing %q:\n%s", want, captured)
		}
	}
	ordered := memoriesBySource(memories)
	for i, want := range []string{"a-1", "b-1", "c-1", "a-2"} {
		if got := asString(ordered[i]["uri"]); got != want {
			t.Fatalf("balanced memory order[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestJudgeMissingReferenceIsGateFailNotJudgeError(t *testing.T) {
	// a missing reference is a config/mechanics error (deterministic_check_failed),
	// NOT a JudgeError (judge_failed) — and the judge is GATE_CRITICAL, so it must
	// raise even under observe mode.
	t.Setenv("OVTEST_GATE_MODE", "observe")
	_, err := newOp(OvJudge, "judge", map[string]any{}).Process(map[string]any{"reply": "x"})
	var ce *ConfigError
	if err == nil || !errors.As(err, &ce) {
		t.Fatalf("missing reference must be a ConfigError, got %v", err)
	}
}
