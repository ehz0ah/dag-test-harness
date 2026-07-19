package codex

import (
	"os"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestCodexOpenVikingAutomaticMemoryCaseShape(t *testing.T) {
	assertCaseShape(t, "codex-openviking-automatic-memory",
		[]string{"chat_capture", "chat_commit_trigger", "verify_captured_session", "find_extracted_memory", "chat_recall", "reply_check"}, "reply_check")
}

func TestCodexOpenVikingMCPToolsCaseShape(t *testing.T) {
	assertCaseShape(t, "codex-openviking-mcp-tools",
		[]string{"chat_mcp_submit", "mcp_submit_evidence", "find_memory_before_forget", "exact_memory_uri", "wait_for_resource_ingestion", "find_remote_resource", "find_local_resource", "chat_mcp_resource_tools", "mcp_resource_tool_evidence", "chat_mcp_forget_exact_uri", "mcp_forget_evidence", "find_forgotten_memory", "evidence_check"}, "evidence_check")
}

func TestCodexLocalResourceFixtureIsCommitted(t *testing.T) {
	path := codexLocalResourceFixturePath()
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

func TestCodexMCPSubmitPromptCoversSignedLocalUpload(t *testing.T) {
	prompt := codexMCPSubmitPrompt(
		"CXMCP-ABCD",
		"mango sticky rice",
		"new zealand",
		"viking://resources/ovtest-codex/cxmcp-abcd.md",
		"https://example.test/remote.md",
		"remote marker",
		"viking://resources/ovtest-codex/local-cxmcp-abcd.md",
		"/tmp/agent-memory.md",
		"local marker",
	)
	for _, want := range []string{
		"Call OpenViking remember to store this single marker fact:",
		"Marker CXMCP-ABCD has color mango sticky rice and associated food new zealand.",
		"Call OpenViking add_resource on this local file path",
		"signed temp_upload URL",
		`curl -fsS -X POST -F "file=@/tmp/agent-memory.md"`,
		"Do not call add_resource a second time",
		"local marker",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Codex MCP memory marker") {
		t.Fatalf("prompt should use an inert user preference, not a test-rule-shaped marker:\n%s", prompt)
	}
	if strings.Contains(prompt, "preference ticket") {
		t.Fatalf("prompt should not use unnatural preference-ticket wording:\n%s", prompt)
	}
	if strings.Contains(prompt, "preferred dashboard accent color") {
		t.Fatalf("prompt should use favourite food and country, not dashboard color:\n%s", prompt)
	}
	if strings.Contains(strings.ToLower(prompt), "forget") {
		t.Fatalf("submit prompt must not delete memory before the external positive gate:\n%s", prompt)
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
