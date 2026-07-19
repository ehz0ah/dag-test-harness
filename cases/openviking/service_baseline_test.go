package openviking

import (
	"reflect"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
)

func TestServiceBaselineWiresStaticMessagesAsInput(t *testing.T) {
	builder := dag.New()
	serviceBaselineCase().Build(builder)
	workflow, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := dag.NewExecutor(workflow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"session_message_payload", "session_new", "user"}
	if got := executor.Dependencies()["session_messages"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("session_messages dependencies = %v, want %v", got, want)
	}
	if !workflow.Terminal("evidence_check") {
		t.Fatal("service baseline must terminate at deterministic evidence")
	}
}

func TestPromotedOpenVikingCasesAvoidGlobalWait(t *testing.T) {
	for _, id := range []string{
		"ov-memory-update",
		"ov-experience-learning",
		"ov-negative-recall",
		"ov-retrieval-precision",
		"ov-memory-cjk",
	} {
		var targetFound bool
		for _, c := range All() {
			if c.ID != id {
				continue
			}
			targetFound = true
			builder := dag.New()
			c.Build(builder)
			workflow, err := builder.Build()
			if err != nil {
				t.Fatalf("%s build: %v", id, err)
			}
			for _, node := range workflow.Nodes() {
				if node == "wait" || len(node) > 5 && node[:5] == "wait_" {
					t.Fatalf("%s still uses global wait node %q", id, node)
				}
			}
		}
		if !targetFound {
			t.Fatalf("case %s is not registered", id)
		}
	}
}

func TestRetrievalPrecisionUsesOneCoherentSession(t *testing.T) {
	builder := dag.New()
	ovRetrievalPrecisionCase().Build(builder)
	workflow, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := dag.NewExecutor(workflow)
	if err != nil {
		t.Fatal(err)
	}
	var sessions, commits int
	for _, node := range workflow.Nodes() {
		if len(node) >= len("session_new_") && node[:len("session_new_")] == "session_new_" {
			sessions++
		}
		if len(node) >= len("commit_") && node[:len("commit_")] == "commit_" {
			commits++
		}
	}
	if sessions != 1 || commits != 1 {
		t.Fatalf("retrieval fixture sessions=%d commits=%d, want one coherent session and commit; nodes=%v", sessions, commits, workflow.Nodes())
	}
	if deps := executor.Dependencies()["find_seeded_corpus"]; !containsString(deps, "commit_schedule") {
		t.Fatalf("find_seeded_corpus dependencies = %v, want commit_schedule", deps)
	}
}
