package ops

import (
	"errors"
	"fmt"
	"testing"
)

func TestSessionCommittedDoesNotIssueCommit(t *testing.T) {
	var calls [][]string
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		calls = append(calls, append([]string(nil), args...))
		if equalStrs(args, []string{"session", "commit", "sid-1"}) {
			t.Fatalf("passive committed gate must not call session commit")
		}
		return okJSON(map[string]any{
			"uri":          "viking://session/sid-1",
			"commit_count": 1,
			"memories_extracted": map[string]any{
				"total": 1,
			},
			"archive_uri": "viking://session/sid-1/archive.json",
		})
	})()

	out, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 0,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil {
		t.Fatalf("committed gate should pass: %v", err)
	}
	if len(calls) != 1 || !equalStrs(calls[0], []string{"session", "get", "sid-1"}) {
		t.Fatalf("calls = %v", calls)
	}
	if out["ok"] != true || out["attempts"] != 1 || out["commit_count"] != 1 {
		t.Fatalf("out = %v", out)
	}
}

func TestSessionCommittedPollsUntilAutoCommitReady(t *testing.T) {
	attempts := 0
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		if !equalStrs(args, []string{"session", "get", "sid-1"}) {
			t.Fatalf("unexpected args: %v", args)
		}
		attempts++
		if attempts == 1 {
			return okJSON(map[string]any{"commit_count": 0, "memories_extracted": map[string]any{"total": 0}})
		}
		return okJSON(map[string]any{"uri": "viking://session/sid-1", "commit_count": 1, "memories_extracted": map[string]any{"total": 1}})
	})()

	out, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 0, "retry": 2,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil {
		t.Fatalf("committed gate should eventually pass: %v", err)
	}
	if out["attempts"] != 2 {
		t.Fatalf("attempts = %v, want 2", out["attempts"])
	}
}

func TestSessionCommittedFailsWhenExtractionNeverAppears(t *testing.T) {
	defer stubOv(func([]string, string, int) cliResult {
		return okJSON(map[string]any{"commit_count": 1, "memories_extracted": map[string]any{"total": 0}})
	})()

	_, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 0, "retry": 0,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	var gf *GateFail
	if !errors.As(err, &gf) || !contains(gf.Detail, "not committed/extracted") {
		t.Fatalf("not-ready session must be a gate fail, got %v", err)
	}
}

func TestSessionCommittedPollsExactPluginCommitTaskAndClaimsItsDiff(t *testing.T) {
	statusCalls := 0
	statusSettles := []int{}
	defer stubOv(func(args []string, _ string, settle int) cliResult {
		switch {
		case equalStrs(args, []string{"task", "list", "--task-type", "session_commit"}):
			return okJSON([]map[string]any{
				{"task_id": "other", "task_type": "session_commit", "resource_id": "other-session", "created_at": 20},
				{"task_id": "task-1", "task_type": "session_commit", "resource_id": "sid-1", "created_at": 10},
			})
		case equalStrs(args, []string{"task", "status", "task-1"}):
			statusSettles = append(statusSettles, settle)
			statusCalls++
			if statusCalls == 1 {
				return okJSON(map[string]any{"task_id": "task-1", "status": "running"})
			}
			return okJSON(map[string]any{
				"task_id": "task-1", "status": "completed",
				"result": map[string]any{
					"archive_uri":        "viking://user/u/sessions/sid-1/history/archive_001",
					"memory_diff_uri":    "viking://user/u/sessions/sid-1/history/archive_001/memory_diff.json",
					"memories_extracted": map[string]any{"memory_write": 1},
				},
			})
		case equalStrs(args, []string{"session", "get", "sid-1"}):
			return okJSON(map[string]any{
				"uri": "viking://user/u/sessions/sid-1", "commit_count": 1,
			})
		case equalStrs(args, []string{"read", "viking://user/u/sessions/sid-1/history/archive_001/memory_diff.json"}):
			return okJSON(`{"operations":{"adds":[{"uri":"viking://user/u/memories/preferences/new.md"}]}}`)
		default:
			t.Fatalf("unexpected args: %v", args)
			return cliResult{ExitCode: 1}
		}
	})()

	out, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 5, "retry": 2, "poll_commit_task": true, "cleanup_added_memories": true,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil {
		t.Fatal(err)
	}
	if out["task_id"] != "task-1" || out["attempts"] != 2 {
		t.Fatalf("out = %v", out)
	}
	if !equalInts(statusSettles, []int{0, 5, 0}) {
		t.Fatalf("status settle values = %v, want discovery wait, one poll interval, then exact-task aggregation", statusSettles)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 2 {
		t.Fatalf("cleanup claims = %#v", out[CleanupClaimsOutput])
	}
	if claims[0].URI != "viking://user/u/sessions/sid-1" || claims[1].URI != "viking://user/u/memories/preferences/new.md" {
		t.Fatalf("cleanup claims = %#v", claims)
	}
}

func TestSessionCommittedAggregatesAllExactSessionCommitTasks(t *testing.T) {
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		switch {
		case equalStrs(args, []string{"task", "list", "--task-type", "session_commit"}):
			return okJSON([]map[string]any{
				{"task_id": "assistant-task", "task_type": "session_commit", "resource_id": "sid-1"},
				{"task_id": "user-task", "task_type": "session_commit", "resource_id": "sid-1"},
				{"task_id": "other-task", "task_type": "session_commit", "resource_id": "sid-10"},
			})
		case equalStrs(args, []string{"task", "status", "assistant-task"}):
			return okJSON(map[string]any{"task_id": "assistant-task", "status": "completed", "result": map[string]any{
				"archive_uri":        "viking://user/u/sessions/sid-1/history/archive_002",
				"memory_diff_uri":    "viking://user/u/sessions/sid-1/history/archive_002/memory_diff.json",
				"memories_extracted": map[string]any{},
			}})
		case equalStrs(args, []string{"task", "status", "user-task"}):
			return okJSON(map[string]any{"task_id": "user-task", "status": "completed", "result": map[string]any{
				"archive_uri":        "viking://user/u/sessions/sid-1/history/archive_001",
				"memory_diff_uri":    "viking://user/u/sessions/sid-1/history/archive_001/memory_diff.json",
				"memories_extracted": map[string]any{"memory_write": 2},
			}})
		case equalStrs(args, []string{"session", "get", "sid-1"}):
			return okJSON(map[string]any{"uri": "viking://user/u/sessions/sid-1", "commit_count": 2})
		case equalStrs(args, []string{"read", "viking://user/u/sessions/sid-1/history/archive_002/memory_diff.json"}):
			return okJSON(`{"operations":{"adds":[]}}`)
		case equalStrs(args, []string{"read", "viking://user/u/sessions/sid-1/history/archive_001/memory_diff.json"}):
			return okJSON(`{"operations":{"adds":[{"uri":"viking://user/u/memories/preferences/alias.md"},{"uri":"viking://user/u/memories/preferences/color.md"}]}}`)
		default:
			t.Fatalf("unexpected args: %v", args)
			return cliResult{ExitCode: 1}
		}
	})()

	out, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 0, "retry": 0, "poll_commit_task": true, "cleanup_added_memories": true,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	if err != nil {
		t.Fatal(err)
	}
	extracted, _ := out["extracted"].(map[string]any)
	if asInt(extracted["total"], 0) != 2 || out["commit_count"] != 2 {
		t.Fatalf("out = %#v", out)
	}
	claims, ok := out[CleanupClaimsOutput].([]CleanupClaim)
	if !ok || len(claims) != 3 {
		t.Fatalf("cleanup claims = %#v", out[CleanupClaimsOutput])
	}
}

func equalInts(a, b []int) bool {
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

func TestSessionCommittedPluginTaskFailsFast(t *testing.T) {
	calls := 0
	defer stubOv(func(args []string, _ string, _ int) cliResult {
		calls++
		switch {
		case equalStrs(args, []string{"task", "list", "--task-type", "session_commit"}):
			return okJSON([]map[string]any{{
				"task_id": "task-1", "task_type": "session_commit", "resource_id": "sid-1",
			}})
		case equalStrs(args, []string{"task", "status", "task-1"}):
			return okJSON(map[string]any{"task_id": "task-1", "status": "failed", "error": "extractor stopped"})
		default:
			t.Fatalf("unexpected args: %v", args)
			return cliResult{ExitCode: 1}
		}
	})()

	_, err := newOp(OvSessionCommitted, "committed", map[string]any{
		"settle": 0, "retry": 10, "poll_commit_task": true,
	}).Process(map[string]any{"user_key": "uk", "session_id": "sid-1"})
	var gf *GateFail
	if !errors.As(err, &gf) || !contains(gf.Detail, "extractor stopped") {
		t.Fatalf("failed task must be a gate fail, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want fail-fast discovery and status only", calls)
	}
}

func TestLatestSessionCommitTaskUsesExactResourceAndServerOrder(t *testing.T) {
	task := latestSessionCommitTask([]map[string]any{
		{"task_id": "wrong-type", "task_type": "add_resource", "resource_id": "sid-1", "created_at": 30},
		{"task_id": "new", "task_type": "session_commit", "resource_id": "sid-1", "created_at": 20},
		{"task_id": "old", "task_type": "session_commit", "resource_id": "sid-1", "created_at": 10},
		{"task_id": "other", "task_type": "session_commit", "resource_id": "sid-10", "created_at": 40},
	}, "sid-1")
	if got := fmt.Sprint(task["task_id"]); got != "new" {
		t.Fatalf("task_id = %q, want newest exact-session task", got)
	}
}
