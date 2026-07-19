package opencode

import (
	"os"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestOpenCodeOpenVikingAutomaticMemoryCaseShape(t *testing.T) {
	assertCaseShape(t, "opencode-openviking-automatic-memory",
		[]string{"chat_capture", "verify_captured_session", "find_extracted_alias", "find_extracted_color", "chat_recall", "reply_check"}, "reply_check")
}

func TestOpenCodeOpenVikingMCPToolsCaseShape(t *testing.T) {
	assertCaseShape(t, "opencode-openviking-mcp-tools",
		[]string{"chat_mcp_memory", "mcp_memory_evidence", "chat_mcp_local_resource", "mcp_local_resource_evidence", "chat_mcp_remote_resource", "mcp_remote_resource_evidence", "find_memory_before_forget", "exact_memory_uri", "wait_for_resource_ingestion", "find_remote_resource", "find_local_resource", "chat_mcp_forget_exact_uri", "mcp_forget_evidence", "find_forgotten_memory", "evidence_check"}, "evidence_check")
}

func TestOpenCodeLocalResourceFixtureIsCommitted(t *testing.T) {
	path := opencodeLocalResourceFixturePath()
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

func TestOpenCodeMCPPromptsAreFocusedAndPreserveExactResourceArguments(t *testing.T) {
	memory := opencodeMCPMemoryPrompt("OCMCP-ABCD", "mango sticky rice", "new zealand")
	for _, want := range []string{"Call remember once", "Call find once", "Call search once", "OPENCODE_MCP_MEMORY_PASS OCMCP-ABCD"} {
		if !strings.Contains(memory, want) {
			t.Fatalf("memory prompt missing %q:\n%s", want, memory)
		}
	}

	local := opencodeMCPLocalResourcePrompt("/tmp/agent-memory.md", "viking://resources/r/local.md", "local marker")
	for _, want := range []string{`path="/tmp/agent-memory.md"`, `to="viking://resources/r/local.md"`, "Copy both values verbatim", `curl -fsS -X POST -F "file=@/tmp/agent-memory.md"`, "OPENCODE_MCP_LOCAL_PASS local marker"} {
		if !strings.Contains(local, want) {
			t.Fatalf("local prompt missing %q:\n%s", want, local)
		}
	}

	remote := opencodeMCPRemoteResourcePrompt("https://example.test/remote.md", "viking://resources/r/remote.md", "viking://resources/r", "remote marker")
	for _, want := range []string{`path="https://example.test/remote.md"`, `to="viking://resources/r/remote.md"`, `target_uri="viking://resources/r"`, "Call list once", "Call glob once", "OPENCODE_MCP_REMOTE_PASS remote marker"} {
		if !strings.Contains(remote, want) {
			t.Fatalf("remote prompt missing %q:\n%s", want, remote)
		}
	}
}

func TestOpenCodeAutomaticPromptsForbidToolBypass(t *testing.T) {
	capture := opencodeAutomaticCapturePrompt("OCAUTO-ABCD", "saffron nickel")
	for _, want := range []string{"ordinary conversation", "stable personal preferences", "retained for future chats", "Do not call any tools or memory functions", "OCAUTO-ABCD", "saffron nickel"} {
		if !strings.Contains(capture, want) {
			t.Fatalf("capture prompt missing %q: %s", want, capture)
		}
	}
	recall := opencodeAutomaticRecallPrompt()
	for _, want := range []string{"context already supplied automatically", "project alias", "preferred theme color", "Do not call any tools or memory functions", "only the alias and color"} {
		if !strings.Contains(recall, want) {
			t.Fatalf("recall prompt missing %q: %s", want, recall)
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
