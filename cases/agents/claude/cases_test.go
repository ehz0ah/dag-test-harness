package claude

import (
	"os"
	"strings"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/runner"
)

func TestClaudeCodeOpenVikingAutomaticMemoryCaseShape(t *testing.T) {
	assertCaseShape(t, "claude-code-openviking-automatic-memory",
		[]string{"chat_capture", "verify_captured_session", "find_extracted_memory", "chat_recall", "reply_check"}, "reply_check")
}

func TestClaudeCodeAutomaticMemoryUsesRecallSupportedPreference(t *testing.T) {
	capture := strings.ToLower(claudeAutomaticMemoryCapturePrompt("CLAUTO-ABC", "violet bronze"))
	recall := strings.ToLower(claudeAutomaticMemoryRecallPrompt())
	for _, want := range []string{"prefer", "personal project", "nickname", "clauto-abc", "violet bronze"} {
		if !strings.Contains(capture, want) {
			t.Fatalf("capture prompt missing %q: %s", want, capture)
		}
	}
	for _, want := range []string{"prefer", "personal project", "nickname", "color"} {
		if !strings.Contains(recall, want) {
			t.Fatalf("recall prompt missing %q: %s", want, recall)
		}
	}
	for _, unsupported := range []string{"profile note", "codename", "badge"} {
		if strings.Contains(capture, unsupported) || strings.Contains(recall, unsupported) {
			t.Fatalf("automatic recall fixture must not depend on unsupported profile retrieval: %q", unsupported)
		}
	}
}

func TestClaudeCodeOpenVikingMCPToolsCaseShape(t *testing.T) {
	assertCaseShape(t, "claude-code-openviking-mcp-tools",
		[]string{"chat_mcp_tools", "mcp_tool_evidence", "find_memory_before_forget", "exact_memory_uri", "wait_for_resource_ingestion", "find_remote_resource", "find_local_resource", "chat_mcp_forget_exact_uri", "mcp_forget_evidence", "find_forgotten_memory", "evidence_check"}, "evidence_check")
}

func TestClaudeCodeOpenVikingSubagentCaseShape(t *testing.T) {
	assertCaseShape(t, "claude-code-openviking-subagent-lifecycle",
		[]string{"run_child_agent", "subagent_hook_evidence", "wait_for_child_commit", "verify_parent_session", "verify_child_session", "find_child_memory", "evidence_check"}, "evidence_check")
}

func TestClaudeLocalResourceFixtureIsCommitted(t *testing.T) {
	path := claudeLocalResourceFixturePath()
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

func TestClaudeMCPToolsPromptCoversSingleFlow(t *testing.T) {
	prompt := claudeMCPToolsPrompt(
		"CLMCP-ABCD",
		"mango sticky rice",
		"new zealand",
		"viking://resources/ovtest-runs/test/claude-code",
		"viking://resources/ovtest-claude/clmcp-abcd.md",
		"https://example.test/remote.md",
		"remote marker",
		"viking://resources/ovtest-claude/local-clmcp-abcd.md",
		"/tmp/agent-memory.md",
		"local marker",
	)
	for _, want := range []string{
		"Call OpenViking health.",
		"Call OpenViking remember to store this single marker fact:",
		"Marker CLMCP-ABCD has color mango sticky rice and associated food new zealand.",
		"Call OpenViking add_resource on this remote URL: https://example.test/remote.md",
		"Call OpenViking add_resource on this local file path: /tmp/agent-memory.md",
		"signed temp_upload URL",
		`curl -fsS -X POST -F "file=@/tmp/agent-memory.md"`,
		"Call OpenViking list on",
		"Call OpenViking grep for",
		"Call OpenViking glob with pattern",
		"CLAUDE_MCP_TOOLS_PASS CLMCP-ABCD",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "preference ticket") || strings.Contains(prompt, "dashboard accent color") {
		t.Fatalf("prompt should use natural user preference wording:\n%s", prompt)
	}
	if strings.Contains(strings.ToLower(prompt), "forget") {
		t.Fatalf("tool prompt must not delete memory before the external positive gate:\n%s", prompt)
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
