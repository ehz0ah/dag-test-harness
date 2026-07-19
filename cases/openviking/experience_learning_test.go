package openviking

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
)

func TestExperienceLearningFixtureIsOpenVikingScopedAndTaskLike(t *testing.T) {
	path := experienceLearningTestdataPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var rawObject map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawObject); err != nil {
		t.Fatalf("parse fixture object: %v", err)
	}
	for _, removed := range []string{"description", "message_contract", "recommended_memory_policy"} {
		if _, ok := rawObject[removed]; ok {
			t.Fatalf("fixture should stay lightweight; remove top-level %q", removed)
		}
	}
	var rawScenarios []map[string]json.RawMessage
	if err := json.Unmarshal(rawObject["scenarios"], &rawScenarios); err != nil {
		t.Fatalf("parse raw scenarios: %v", err)
	}
	for _, scenario := range rawScenarios {
		var id string
		_ = json.Unmarshal(scenario["id"], &id)
		for _, removed := range []string{"signal_type", "objective", "expected_lesson", "retrieval_query", "required_concepts", "forbidden_concepts"} {
			if _, ok := scenario[removed]; ok {
				t.Fatalf("%s should stay lightweight; remove %q", id, removed)
			}
		}
	}

	var fixture struct {
		ID           string         `json:"id"`
		MemoryPolicy map[string]any `json:"memory_policy"`
		Scenarios    []struct {
			ID       string `json:"id"`
			Query    string `json:"query"`
			Expected string `json:"expected"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if fixture.ID != "agent_experience_learning_v1" {
		t.Fatalf("fixture id = %q", fixture.ID)
	}
	if len(fixture.MemoryPolicy) == 0 {
		t.Fatalf("fixture must include memory_policy")
	}
	memoryTypes, ok := fixture.MemoryPolicy["memory_types"].([]any)
	if !ok || !reflect.DeepEqual(memoryTypes, []any{"cases", "trajectories", "experiences"}) {
		t.Fatalf("memory_policy.memory_types = %#v", fixture.MemoryPolicy["memory_types"])
	}
	if len(fixture.Scenarios) != 3 {
		t.Fatalf("scenarios = %d, want 3", len(fixture.Scenarios))
	}
	wantIDs := map[string]bool{
		"structured_tool_error_recovery": true,
		"wrong_target_user_correction":   true,
		"guided_workflow_demonstration":  true,
	}
	for _, scenario := range fixture.Scenarios {
		if !wantIDs[scenario.ID] {
			t.Fatalf("unexpected scenario %q", scenario.ID)
		}
		delete(wantIDs, scenario.ID)
		if scenario.Query == "" {
			t.Fatalf("%s has empty query", scenario.ID)
		}
		query := strings.ToLower(scenario.Query)
		if strings.Contains(query, "what should i do") || strings.Contains(query, "how should i") {
			t.Fatalf("%s query is model-thinking shaped: %q", scenario.ID, scenario.Query)
		}
		if len(scenario.Messages) < 6 {
			t.Fatalf("%s has %d messages, want a complete task transcript", scenario.ID, len(scenario.Messages))
		}
		var hasTool bool
		for _, msg := range scenario.Messages {
			hasTool = hasTool || msg.Role == "tool"
		}
		if !hasTool {
			t.Fatalf("%s must include tool observations", scenario.ID)
		}
		lesson := strings.ToLower(scenario.Expected)
		if lesson == "" {
			t.Fatalf("%s has empty expected rubric", scenario.ID)
		}
		switch scenario.ID {
		case "structured_tool_error_recovery":
			for _, token := range []string{"calendar", "date", "retry"} {
				if !strings.Contains(lesson, token) {
					t.Fatalf("%s expected rubric must be calendar-task specific and include %q: %q",
						scenario.ID, token, scenario.Expected)
				}
			}
		case "wrong_target_user_correction":
			for _, token := range []string{"do not assume", "clarify", "before"} {
				if !strings.Contains(lesson, token) {
					t.Fatalf("%s expected rubric must be user-clarification specific and include %q: %q",
						scenario.ID, token, scenario.Expected)
				}
			}
		case "guided_workflow_demonstration":
			for _, token := range []string{"vendor security review", "legal entity", "dpa"} {
				if !strings.Contains(lesson, token) {
					t.Fatalf("%s expected rubric must be vendor-review specific and include %q: %q",
						scenario.ID, token, scenario.Expected)
				}
			}
		}
	}
	if len(wantIDs) > 0 {
		t.Fatalf("missing scenarios: %v", wantIDs)
	}
}

func TestOpenVikingExperienceLearningCaseShape(t *testing.T) {
	cases := map[string]bool{}
	var targetFound bool
	for _, c := range All() {
		cases[c.ID] = true
		if c.ID != "ov-experience-learning" {
			continue
		}
		targetFound = true
		b := dag.New()
		c.Build(b)
		workflow, err := b.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		for _, name := range []string{
			"user",
			"experience_fixture_summary",
			"judge",
		} {
			if !hasOpenVikingNode(workflow.Nodes(), name) {
				t.Fatalf("case is missing node %q; nodes=%v", name, workflow.Nodes())
			}
		}
		for _, id := range []string{
			"structured_tool_error_recovery",
			"wrong_target_user_correction",
			"guided_workflow_demonstration",
		} {
			for _, name := range []string{
				"experience_fixture_" + id,
				"session_new_" + id,
				"seed_transcript_" + id,
				"commit_" + id,
				"find_trajectories_" + id,
				"find_experiences_" + id,
			} {
				if !hasOpenVikingNode(workflow.Nodes(), name) {
					t.Fatalf("case is missing scenario node %q; nodes=%v", name, workflow.Nodes())
				}
			}
		}
		if !workflow.Terminal("judge") {
			t.Fatalf("judge must be terminal")
		}
	}
	if !targetFound {
		t.Fatalf("ov-experience-learning is not registered; cases=%v", cases)
	}
}

func TestExperienceLearningSerializesScenarioCommits(t *testing.T) {
	builder := dag.New()
	ovExperienceLearningCase().Build(builder)
	workflow, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := dag.NewExecutor(workflow)
	if err != nil {
		t.Fatal(err)
	}
	deps := executor.Dependencies()
	order := []string{
		"structured_tool_error_recovery",
		"wrong_target_user_correction",
		"guided_workflow_demonstration",
	}
	for i := 1; i < len(order); i++ {
		previous := "commit_" + order[i-1]
		current := "session_new_" + order[i]
		if !containsString(deps[current], previous) {
			t.Fatalf("%s dependencies = %v, want %s", current, deps[current], previous)
		}
	}
}

func TestExperienceLearningPreservesHarnessToolCallAndResultSequence(t *testing.T) {
	scenario := experienceScenario{
		ID: "tool_sequence",
		Messages: []experienceMessage{
			{Role: "assistant", Content: "calling", ToolCall: &experienceToolCall{
				ID: "call-1", Name: "calendar_create", Arguments: map[string]any{"date": "next Friday"},
			}},
			{Role: "tool", ToolCallID: "call-1", Name: "calendar_create", Content: map[string]any{
				"status": "error", "error_type": "validation_error",
			}},
		},
	}
	messages, err := scenario.sessionMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0]["role"] != "assistant" || messages[1]["role"] != "user" {
		t.Fatalf("tool sequence = %#v", messages)
	}
	call := messages[0]["parts"].([]map[string]any)[1]
	result := messages[1]["parts"].([]map[string]any)[0]
	if call["tool_status"] != "running" || result["tool_status"] != "error" || result["tool_output"] == "" {
		t.Fatalf("call=%v result=%v", call, result)
	}
}

func TestScopedExperienceScenariosUseOfficialTrainingEnvelope(t *testing.T) {
	data := mustLoadExperienceCaseData()
	scenarios := scopedExperienceScenarios(data.Scenarios[:1], "OVEXP-ABC123")
	if len(scenarios) != 1 || len(scenarios[0].Messages) < 3 {
		t.Fatalf("scoped scenarios = %#v", scenarios)
	}
	first := scenarios[0].Messages[0]
	last := scenarios[0].Messages[len(scenarios[0].Messages)-1]
	if first["role"] != "system" || !strings.HasPrefix(messagePartText(t, first), "# OpenViking Batch Training CaseSpec v1") {
		t.Fatalf("first training message = %#v", first)
	}
	if last["role"] != "user" || !strings.HasPrefix(messagePartText(t, last), "# OpenViking OutcomeEvaluation") {
		t.Fatalf("last training message = %#v", last)
	}
	for _, want := range []string{"openviking.batch_train.case_spec.v1", "ovexp-abc123", scenarios[0].ID} {
		if !strings.Contains(strings.ToLower(messagePartText(t, first)), strings.ToLower(want)) {
			t.Fatalf("CaseSpec is missing %q: %s", want, messagePartText(t, first))
		}
	}
}

func messagePartText(t *testing.T, message map[string]any) string {
	t.Helper()
	parts, ok := message["parts"].([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("message parts = %#v", message["parts"])
	}
	text, ok := parts[0]["text"].(string)
	if !ok {
		t.Fatalf("message text = %#v", parts[0]["text"])
	}
	return text
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestOpenVikingExperienceLearningReferenceRequiresEveryScenarioInJudgeExplanation(t *testing.T) {
	ref := strings.ToLower(ovExperienceLearningReference)
	for _, want := range []string{
		"explanation must address all three scenario ids",
		"structured_tool_error_recovery",
		"wrong_target_user_correction",
		"guided_workflow_demonstration",
	} {
		if !strings.Contains(ref, want) {
			t.Fatalf("judge reference should require %q; reference=%s", want, ovExperienceLearningReference)
		}
	}
}

func hasOpenVikingNode(nodes []string, name string) bool {
	for _, node := range nodes {
		if node == name {
			return true
		}
	}
	return false
}

func experienceLearningTestdataPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "experience_learning_v1.json")
}
