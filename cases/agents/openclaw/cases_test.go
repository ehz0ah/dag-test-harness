package openclaw

import (
	"reflect"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
)

func TestAutomaticMemoryUsesDurablePreferenceFixture(t *testing.T) {
	prompt := strings.ToLower(openclawCapturePrompt("Basalt-ABCD"))
	for _, want := range []string{"stable personal preferences", "retained for future chats", "project alias", "preferred theme color", "basalt-abcd", openclawAccent} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("capture prompt missing %q: %s", want, prompt)
		}
	}
	for _, unstable := range []string{"launch date", "march 14", "2027"} {
		if strings.Contains(prompt, unstable) {
			t.Fatalf("capture prompt contains unstable event fixture %q: %s", unstable, prompt)
		}
	}
}

func TestToolMemoryExpectToleratesExtractionPunctuation(t *testing.T) {
	want := []string{"clawtool-ab12", "amber", "violet"}
	if got := toolMemoryExpect("CLAWTOOL-AB12"); !reflect.DeepEqual(got, want) {
		t.Fatalf("toolMemoryExpect() = %v, want %v", got, want)
	}
}

func TestReleaseGateCaseShapes(t *testing.T) {
	tests := []struct {
		id       string
		nodes    []string
		terminal string
	}{
		{"openclaw-openviking-automatic-memory", []string{"chat_capture", "capture_no_tool_bypass", "verify_captured_session", "find_extracted_alias", "find_extracted_color", "chat_recall", "recall_no_tool_bypass", "reply_check"}, "reply_check"},
		{"openclaw-openviking-tools", []string{"chat_memory_store", "memory_store_evidence", "find_memory_before_forget", "exact_memory_uri", "chat_memory_recall", "memory_recall_evidence", "chat_memory_search", "memory_search_evidence", "chat_memory_read", "memory_read_evidence", "chat_local_resource", "local_resource_evidence", "chat_remote_resource", "remote_resource_evidence", "find_local_resource", "find_remote_resource", "chat_forget_exact_uri", "forget_evidence", "find_memory_after_forget", "evidence_check"}, "evidence_check"},
		{"openclaw-openviking-compaction", []string{"chat_capture", "capture_no_tool_bypass", "compact_session", "verify_compacted_session", "find_compacted_memory", "chat_after_compaction", "recall_no_tool_bypass", "reply_check"}, "reply_check"},
	}
	cases := map[string]func(*dag.Builder){}
	for _, testCase := range All() {
		cases[testCase.ID] = testCase.Build
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			build := cases[test.id]
			if build == nil {
				t.Fatalf("case is not registered")
			}
			builder := dag.New()
			build(builder)
			workflow, err := builder.Build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			for _, node := range test.nodes {
				found := false
				for _, got := range workflow.Nodes() {
					found = found || got == node
				}
				if !found {
					t.Fatalf("missing node %q; nodes=%v", node, workflow.Nodes())
				}
			}
			if !workflow.Terminal(test.terminal) {
				t.Fatalf("%q must be terminal", test.terminal)
			}
		})
	}
}
