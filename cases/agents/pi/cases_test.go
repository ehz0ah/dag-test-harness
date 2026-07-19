package pi

import (
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestPiAutomaticMemoryCaseShape(t *testing.T) {
	assertCaseShape(t, "pi-openviking-automatic-memory", []string{
		"chat_capture", "capture_no_tool_bypass", "verify_captured_session",
		"find_extracted_memory", "chat_recall", "recall_no_tool_bypass", "reply_check",
	}, "reply_check")
}

func TestPiAutomaticMemoryUsesSupportedPreferenceWording(t *testing.T) {
	capture := strings.ToLower(piAutomaticMemoryCapturePrompt("PIAUTO-ABC", "copper lilac"))
	recall := strings.ToLower(piAutomaticMemoryRecallPrompt())
	for _, want := range []string{"prefer", "coding workspace", "scratch-project", "piauto-abc", "copper lilac"} {
		if !strings.Contains(capture, want) {
			t.Fatalf("capture prompt missing %q: %s", want, capture)
		}
	}
	for _, forbidden := range []string{"profile fact", "field alias", "badge color"} {
		if strings.Contains(capture, forbidden) || strings.Contains(recall, forbidden) {
			t.Fatalf("automatic memory fixture retained unsupported wording %q", forbidden)
		}
	}
}

func TestPiToolsCaseShape(t *testing.T) {
	assertCaseShape(t, "pi-openviking-tools", []string{
		"remember_memory", "remember_evidence", "verify_remembered_session",
		"find_memory_before_forget", "exact_memory_uri", "native_tool_workflow",
		"native_tool_evidence", "wait_for_resource", "find_remote_resource",
		"verify_memory_forgotten", "evidence_check",
	}, "evidence_check")
}

func TestPiTakeoverCaseShape(t *testing.T) {
	assertCaseShape(t, "pi-openviking-takeover-compaction", []string{
		"capture_and_compact", "takeover_compaction_evidence", "verify_takeover_session",
		"find_takeover_memory", "recall_after_takeover", "recall_no_tool_bypass", "reply_check",
	}, "reply_check")
}

func assertCaseShape(t *testing.T, id string, wantNodes []string, terminal string) {
	t.Helper()
	c, ok := caseMap()[id]
	if !ok {
		t.Fatalf("%s is not registered", id)
	}
	b := dag.New()
	c.Build(b)
	workflow, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, name := range wantNodes {
		if !hasNode(workflow.Nodes(), name) {
			t.Fatalf("case %s is missing node %q; nodes=%v", id, name, workflow.Nodes())
		}
	}
	if !workflow.Terminal(terminal) {
		t.Fatalf("%s must be terminal", terminal)
	}
}

func hasNode(nodes []string, name string) bool {
	for _, node := range nodes {
		if node == name {
			return true
		}
	}
	return false
}

func caseMap() map[string]runner.Case {
	out := make(map[string]runner.Case)
	for _, c := range All() {
		out[c.ID] = c
	}
	return out
}
