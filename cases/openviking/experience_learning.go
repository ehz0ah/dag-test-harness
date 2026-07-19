package openviking

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

const ovExperienceLearningGoal = "OpenViking extracts reusable agent trajectories and experiences " +
	"from visible human-assistant/tool transcripts, stores them under the trajectory/experience memory " +
	"types, and can retrieve experience records for future task-like situations."

// Experience extraction can legitimately take several minutes because these
// training sessions request cases, trajectories, and experiences together.
// Poll the exact commit task for up to 15 minutes; polling returns immediately
// when that task completes, and all extraction/retrieval gates remain exact.
const (
	experienceCommitPollSeconds = 3
	experienceCommitRetries     = 300
)

const ovExperienceLearningReference = `Expected trace:
- each "session_new_<scenario>": creates one OpenViking training session per scenario with
  memory_policy.memory_types limited to cases, trajectories, and experiences.
- each "seed_transcript_<scenario>": batch-adds only that scenario through the session API with
  the official CaseSpec control message first, visible text/tool parts for the rollout, and an
  OutcomeEvaluation training signal last. Hidden model thinking is not part of the contract.
- each "commit_<scenario>": commits that scenario session and waits until asynchronous extraction reports memories extracted.
- each "find_trajectories_<scenario>": retrieves at least one trajectory memory from .../memories/trajectories/ for that scenario.
- each "find_experiences_<scenario>": retrieves at least one experience memory from .../memories/experiences/ using that scenario's future task-like query.

PASS only if the retrieved trajectory/experience evidence itself captures reusable agent behavior from
the fixture: calendar scheduling should resolve relative dates into the tool's required concrete date
format, ambiguous targets should lead to user clarification before state-changing work, and vendor
security reviews should follow the legal-entity -> questionnaire -> DPA-prerequisite workflow. Do not
credit generic model cleverness or the raw transcript alone; the retrieved memories must carry the
lesson.

The verdict explanation must address all three scenario ids explicitly:
- structured_tool_error_recovery
- wrong_target_user_correction
- guided_workflow_demonstration
If any scenario is missing, off-rubric, or only supported by the raw transcript rather than retrieved
trajectory/experience memories, FAIL and name that scenario.`

type experienceDataset struct {
	ID           string               `json:"id"`
	MemoryPolicy map[string]any       `json:"memory_policy"`
	Scenarios    []experienceScenario `json:"scenarios"`
}

type experienceScenario struct {
	ID       string              `json:"id"`
	Query    string              `json:"query"`
	Expected string              `json:"expected"`
	Messages []experienceMessage `json:"messages"`
}

type experienceMessage struct {
	Role       string              `json:"role"`
	Content    any                 `json:"content"`
	ToolCall   *experienceToolCall `json:"tool_call,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	Name       string              `json:"name,omitempty"`
}

type experienceToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type experienceCaseData struct {
	Policy    map[string]any
	Scenarios []experienceScenarioData
	Summary   map[string]any
}

type experienceScenarioData struct {
	ID       string
	Query    string
	Expected string
	Messages []map[string]any
}

func ovExperienceLearningCase() runner.Case {
	data := mustLoadExperienceCaseData()
	return runner.Case{
		ID:        "ov-experience-learning",
		Goal:      ovExperienceLearningGoal,
		Reference: ovExperienceLearningReference,
		Build: func(b *dag.Builder) {
			marker := "OVEXP-" + strings.ToUpper(nonce(6))
			scenarios := scopedExperienceScenarios(data.Scenarios, marker)
			user := runner.ConfiguredUser(b, "user")
			summary := b.Var(map[string]any{"marker": marker, "fixture": data.Summary}, "experience_fixture_summary")

			var commits []dag.Input
			var finds []dag.Input
			var experienceEvidence []dag.Input
			var trajectoryEvidence []dag.Input
			var previous dag.Input = user
			for _, scenario := range scenarios {
				fixture := b.Var(map[string]any{
					"messages": scenario.Messages,
				}, "experience_fixture_"+scenario.ID)

				session := b.Add(ovops.SessionNew, dag.Spec{
					Name: "session_new_" + scenario.ID, In: dag.In{"user_key": user, "after": previous},
					Config: dag.Cfg{"memory_policy": data.Policy}})
				seed := b.Add(ovops.SessionAddMessages, dag.Spec{
					Name: "seed_transcript_" + scenario.ID,
					In: dag.In{
						"user_key":   user,
						"session_id": session,
						"messages":   fixture.At("messages"),
						"after":      session,
					}})

				commit := b.Add(ovops.SessionCommit, dag.Spec{
					Name: "commit_" + scenario.ID, In: dag.In{"user_key": user, "session_id": session, "after": seed},
					Config: dag.Cfg{
						"min_extracted": 1, "settle": experienceCommitPollSeconds,
						"retry": experienceCommitRetries, "cleanup_added_memories": true,
					}})
				commits = append(commits, commit)
				previous = commit
			}

			afterAllCommits := b.Merge(commits...)
			for _, scenario := range scenarios {
				findTrajectories := b.Add(ovops.Find, dag.Spec{
					Name: "find_trajectories_" + scenario.ID,
					In:   dag.In{"user_key": user, "after": afterAllCommits},
					Config: dag.Cfg{
						"query":       scenario.Query,
						"uri":         "viking://user/memories/trajectories",
						"expect_uri":  "/trajectories/",
						"min_results": 1,
						"settle":      12,
						"retry":       10,
					}})
				findExperiences := b.Add(ovops.Find, dag.Spec{
					Name: "find_experiences_" + scenario.ID,
					In:   dag.In{"user_key": user, "after": afterAllCommits},
					Config: dag.Cfg{
						"query":       scenario.Query,
						"uri":         "viking://user/memories/experiences",
						"expect_uri":  "/experiences/",
						"min_results": 1,
						"settle":      12,
						"retry":       10,
					}})
				finds = append(finds, findTrajectories, findExperiences)
				trajectoryEvidence = append(trajectoryEvidence, findTrajectories.Out("relevant"))
				experienceEvidence = append(experienceEvidence, findExperiences.Out("relevant"))
			}

			b.Add(ovops.Judge, dag.Spec{Name: "judge",
				In: dag.In{
					"created":         user,
					"transcript":      summary,
					"memories":        b.Merge(experienceEvidence...),
					"search_memories": b.Merge(trajectoryEvidence...),
					"after":           b.Merge(append(commits, finds...)...),
				},
				Config: dag.Cfg{"goal": ovExperienceLearningGoal, "reference": ovExperienceLearningReference}})
		},
	}
}

func scopedExperienceScenarios(in []experienceScenarioData, marker string) []experienceScenarioData {
	out := make([]experienceScenarioData, 0, len(in))
	for _, scenario := range in {
		scoped := scenario
		scoped.Query = marker + " " + scenario.Query
		scoped.Messages = make([]map[string]any, len(scenario.Messages))
		marked := false
		for i, message := range scenario.Messages {
			copy := make(map[string]any, len(message))
			for key, value := range message {
				copy[key] = value
			}
			if !marked && copy["role"] == "user" {
				copy["content"] = fmt.Sprintf("Case marker %s. %s", marker, contentString(copy["content"]))
				marked = true
			}
			scoped.Messages[i] = copy
		}
		scoped.Messages = append([]map[string]any{trainingCaseSpecMessage(scoped, marker)}, scoped.Messages...)
		scoped.Messages = append(scoped.Messages, trainingOutcomeMessage(scoped))
		out = append(out, scoped)
	}
	return out
}

func trainingCaseSpecMessage(scenario experienceScenarioData, marker string) map[string]any {
	payload := map[string]any{
		"protocol": "openviking.batch_train.case_spec.v1",
		"case": map[string]any{
			"name":           strings.ToLower(marker + "-" + scenario.ID),
			"task_signature": "ovtest:experience:" + strings.ToLower(marker) + ":" + scenario.ID,
			"input":          map[string]any{"user_query": scenario.Query},
			"metadata":       map[string]any{"scenario_id": scenario.ID, "marker": marker},
			"rubric": map[string]any{
				"name":        scenario.ID + "_rubric",
				"description": scenario.Expected,
				"criteria": []map[string]any{{
					"name": "reusable_behavior", "description": scenario.Expected,
					"required": true, "weight": 1.0,
				}},
			},
		},
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		panic(err)
	}
	text := "# OpenViking Batch Training CaseSpec v1\n\n" +
		"The following structured case and rubric describe the task that produced this rollout. " +
		"It is control-plane metadata for the batch training pipeline.\n\n```json\n" + string(raw) + "\n```"
	return map[string]any{"role": "system", "parts": []map[string]any{{"type": "text", "text": text}}}
}

func trainingOutcomeMessage(scenario experienceScenarioData) map[string]any {
	payload := map[string]any{"evaluation": map[string]any{
		"passed": true, "score": 1.0, "feedback": scenario.Expected,
		"criterion_results": []map[string]any{{
			"criterion_name": "reusable_behavior", "passed": true, "score": 1.0,
			"feedback": scenario.Expected, "evidence": "The visible rollout completed with the documented correction or workflow.",
			"metadata": map[string]any{"scenario_id": scenario.ID},
		}},
		"metadata": map[string]any{"scenario_id": scenario.ID},
	}}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		panic(err)
	}
	text := "# OpenViking OutcomeEvaluation\n\n" +
		"The following structured evaluation describes the outcome of the preceding rollout. " +
		"Use it as the training signal when extracting training memories.\n\n```json\n" + string(raw) + "\n```"
	return map[string]any{"role": "user", "parts": []map[string]any{{"type": "text", "text": text}}}
}

func mustLoadExperienceCaseData() experienceCaseData {
	data, err := loadExperienceCaseData(experienceFixturePath())
	if err != nil {
		panic(err)
	}
	return data
}

func loadExperienceCaseData(path string) (experienceCaseData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return experienceCaseData{}, fmt.Errorf("read experience fixture: %w", err)
	}
	var dataset experienceDataset
	if err := json.Unmarshal(raw, &dataset); err != nil {
		return experienceCaseData{}, fmt.Errorf("parse experience fixture: %w", err)
	}
	scenarios, err := dataset.caseScenarios()
	if err != nil {
		return experienceCaseData{}, err
	}
	return experienceCaseData{
		Policy:    dataset.MemoryPolicy,
		Scenarios: scenarios,
		Summary:   dataset.summary(),
	}, nil
}

func experienceFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("cases", "openviking", "testdata", "experience_learning_v1.json")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "experience_learning_v1.json")
}

func (d experienceDataset) summary() map[string]any {
	scenarios := make([]map[string]any, 0, len(d.Scenarios))
	for _, scenario := range d.Scenarios {
		scenarios = append(scenarios, map[string]any{
			"id":            scenario.ID,
			"query":         scenario.Query,
			"expected":      scenario.Expected,
			"message_count": len(scenario.Messages),
		})
	}
	return map[string]any{
		"id":        d.ID,
		"scenarios": scenarios,
	}
}

func (d experienceDataset) caseScenarios() ([]experienceScenarioData, error) {
	out := make([]experienceScenarioData, 0, len(d.Scenarios))
	for _, scenario := range d.Scenarios {
		messages, err := scenario.sessionMessages()
		if err != nil {
			return nil, err
		}
		out = append(out, experienceScenarioData{
			ID:       scenario.ID,
			Query:    scenario.Query,
			Expected: scenario.Expected,
			Messages: messages,
		})
	}
	return out, nil
}

func (s experienceScenario) sessionMessages() ([]map[string]any, error) {
	var out []map[string]any
	for i := 0; i < len(s.Messages); i++ {
		msg := s.Messages[i]
		if msg.Role == "tool" {
			return nil, fmt.Errorf("%s: standalone tool result at message %d", s.ID, i+1)
		}
		if msg.Role == "assistant" && msg.ToolCall != nil {
			sessionMsgs, consumed, err := assistantToolMessages(s.ID, msg, s.Messages[i+1:])
			if err != nil {
				return nil, err
			}
			out = append(out, sessionMsgs...)
			i += consumed
			continue
		}
		out = append(out, map[string]any{
			"role":    msg.Role,
			"content": contentString(msg.Content),
		})
	}
	return out, nil
}

func assistantToolMessages(scenarioID string, msg experienceMessage, rest []experienceMessage) ([]map[string]any, int, error) {
	call := msg.ToolCall
	if call.ID == "" || call.Name == "" {
		return nil, 0, fmt.Errorf("%s: assistant tool_call must include id and name", scenarioID)
	}
	callPart := map[string]any{
		"type":        "tool",
		"tool_id":     call.ID,
		"tool_name":   call.Name,
		"tool_input":  call.Arguments,
		"tool_status": "running",
	}
	if len(rest) == 0 || rest[0].Role != "tool" {
		return nil, 0, fmt.Errorf("%s: tool call %q has no following result", scenarioID, call.ID)
	}
	toolResult := rest[0]
	if toolResult.ToolCallID != call.ID {
		return nil, 0, fmt.Errorf("%s: tool result %q does not match call %q",
			scenarioID, toolResult.ToolCallID, call.ID)
	}
	return []map[string]any{
		{
			"role": "assistant",
			"parts": []map[string]any{
				{"type": "text", "text": contentString(msg.Content)},
				callPart,
			},
		},
		{
			"role": "user",
			"parts": []map[string]any{{
				"type":        "tool",
				"tool_id":     call.ID,
				"tool_name":   call.Name,
				"tool_status": experienceToolStatus(toolResult.Content),
				"tool_output": contentString(toolResult.Content),
			}},
		},
	}, 1, nil
}

func experienceToolStatus(content any) string {
	result, _ := content.(map[string]any)
	if strings.EqualFold(contentString(result["status"]), "error") || result["error_type"] != nil {
		return "error"
	}
	return "completed"
}

func contentString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimRight(buf.String(), "\n")
}
