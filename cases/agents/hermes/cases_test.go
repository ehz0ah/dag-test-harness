package hermes

import (
	"os"
	"slices"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestHermesAutomaticPromptAndEvidenceForbidClarification(t *testing.T) {
	prompt := strings.ToLower(hermesCapturePrompt("VX-ORCHID-AB12"))
	for _, phrase := range []string{"standalone operational update", "no clarification is needed", "do not call tools or ask questions"} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("capture prompt missing %q: %q", phrase, prompt)
		}
	}
	if !slices.Contains(hermesAutomaticForbiddenTools(), "clarify") {
		t.Fatal("automatic-memory evidence must reject clarify tool use")
	}
}

func TestHermesOpenVikingSyncTurnCaseShape(t *testing.T) {
	assertCaseShape(t, "hermes-openviking-automatic-memory",
		[]string{"chat_capture", "capture_no_tool_bypass", "wait_for_commit", "find_story_memory", "chat_recall", "recall_no_tool_bypass", "reply_check"}, "reply_check")
}

func TestHermesOpenVikingToolsCaseShape(t *testing.T) {
	assertCaseShape(t, "hermes-openviking-tools",
		[]string{"chat_tool_workflow", "tool_workflow_evidence", "find_memory_before_forget", "find_local_resource", "find_remote_resource", "exact_memory_uri", "chat_forget_exact_uri", "forget_evidence", "find_memory_after_forget", "evidence_check"}, "evidence_check")
}

func TestHermesHasExactlyTwoPrimaryCases(t *testing.T) {
	if got := len(All()); got != 2 {
		t.Fatalf("Hermes must expose two consolidated primary cases, got %d", got)
	}
}

func TestHermesLocalResourceFixtureIsCommitted(t *testing.T) {
	path := localResourceFixturePath()
	if !strings.Contains(path, "/fixtures/resources/agent-memory.md") {
		t.Fatalf("fixture path = %q, want committed fixture path", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed fixture: %v", err)
	}
	text := strings.ToLower(string(raw))
	for _, token := range []string{"opal-river-409", "indigo silver"} {
		if !strings.Contains(text, token) {
			t.Fatalf("committed fixture missing %q", token)
		}
	}
}

func TestHermesRemoteResourceDefaultIsRealTextResource(t *testing.T) {
	t.Setenv("OV_TEST_REMOTE_RESOURCE_URL", "")
	t.Setenv("OV_TEST_REMOTE_RESOURCE_EXPECT", "")
	if got := remoteResourceURL(); got != support.DefaultRemoteResourceURL {
		t.Fatalf("remoteResourceURL() = %q", got)
	}
	expect := strings.Join(remoteResourceExpect(), "\n")
	for _, token := range []string{"celeste fpga", "basys3", "verilog"} {
		if !strings.Contains(expect, token) {
			t.Fatalf("remoteResourceExpect() missing %q: %q", token, expect)
		}
	}
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
	for _, n := range nodes {
		if n == name {
			return true
		}
	}
	return false
}

func caseMap() map[string]runner.Case {
	out := map[string]runner.Case{}
	for _, c := range All() {
		out[c.ID] = c
	}
	return out
}
