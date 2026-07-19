package openviking

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

func serviceBaselineCase() runner.Case {
	return runner.Case{
		ID:        "openviking-service-baseline",
		Goal:      "OpenViking's authenticated service, session extraction, resource ingestion, retrieval, and exact cleanup contracts work end to end.",
		Reference: "Deterministic gates require health, session commit/extraction, exact memory and resource retrieval, exact deletion, and negative verification.",
		Build: func(b *dag.Builder) {
			token := "OVBASE-" + strings.ToUpper(nonce(12))
			memoryExpect := []string{strings.ToLower(token), "copper indigo"}
			resourceExpect := []string{"opal-river-409", "indigo silver"}
			resourceRoot := support.ResourceRoot("openviking-baseline", strings.ToLower(token))
			user := runner.ConfiguredUser(b, "user")
			health := b.Add(ovops.Command, dag.Spec{
				Name: "health", In: dag.In{"user_key": user}, Config: dag.Cfg{"args": []string{"health"}},
			})
			session := b.Add(ovops.SessionNew, dag.Spec{
				Name: "session_new", In: dag.In{"user_key": user, "after": health},
			})
			messagePayload := b.Var([]map[string]any{
				{"role": "user", "content": fmt.Sprintf("The baseline marker is %s and its color is copper indigo.", token)},
				{"role": "assistant", "content": "Acknowledged the baseline marker and color."},
			}, "session_message_payload")
			messages := b.Add(ovops.SessionAddMessages, dag.Spec{
				Name: "session_messages", In: dag.In{"user_key": user, "session_id": session, "messages": messagePayload, "after": session},
			})
			commit := b.Add(ovops.SessionCommit, dag.Spec{
				Name: "session_commit", In: dag.In{"user_key": user, "session_id": session, "after": messages},
				Config: dag.Cfg{"settle": 5, "retry": 18},
			})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_memory", In: dag.In{"user_key": user, "after": commit},
				Config: dag.Cfg{"query": token + " copper indigo", "expect": memoryExpect, "uri": "viking://user/memories", "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(token)},
			})
			resource := b.Add(ovops.Command, dag.Spec{
				Name: "add_local_resource", In: dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"args":   []string{"add-resource", baselineFixturePath(), "--parent-auto-create", resourceRoot, "--wait", "--timeout", "180"},
					"expect": []string{"success"},
				},
			})
			resourceFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_local_resource", In: dag.In{"user_key": user, "after": resource},
				Config: dag.Cfg{"query": strings.Join(resourceExpect, " "), "expect": resourceExpect, "expect_uri": strings.ToLower(resourceRoot), "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot)},
			})
			memoryRemoved := b.Add(ovops.Remove, dag.Spec{
				Name: "remove_exact_memory", In: dag.In{"user_key": user, "memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"abstract_filter": strings.ToLower(token), "all_matches": true},
			})
			resourceRemoved := b.Add(ovops.Remove, dag.Spec{
				Name: "remove_exact_resource", In: dag.In{"user_key": user, "after": resourceFound},
				Config: dag.Cfg{"uri": resourceRoot, "recursive": true},
			})
			memoryGone := b.Add(ovops.Find, dag.Spec{
				Name: "verify_memory_removed", In: dag.In{"user_key": user, "after": memoryRemoved},
				Config: dag.Cfg{"query": token + " copper indigo", "expect": memoryExpect, "uri": "viking://user/memories", "expect_gone": true, "settle": 5, "retry": 12},
			})
			resourceGone := b.Add(ovops.Find, dag.Spec{
				Name: "verify_resource_removed", In: dag.In{"user_key": user, "after": resourceRemoved},
				Config: dag.Cfg{"query": strings.Join(resourceExpect, " "), "expect": resourceExpect, "expect_uri": strings.ToLower(resourceRoot), "expect_gone": true, "settle": 5, "retry": 12},
			})
			b.Add(checks.Deterministic, dag.Spec{
				Name: "evidence_check", In: dag.In{"after": b.Merge(memoryGone.Out("ok"), resourceGone.Out("ok"))},
				Config: dag.Cfg{"explanation": "OpenViking baseline service, session, resource, retrieval, and exact cleanup gates passed"},
			})
		},
	}
}

func baselineFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("fixtures", "resources", "agent-memory.md")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "fixtures", "resources", "agent-memory.md"))
}
