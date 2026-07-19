// Package cases holds the ovtest e2e cases. Each case is built by a `xCase()`
// constructor and assembled into All; the run CLI looks cases up by id.
package cases

import (
	agentclaude "code.byted.org/data-arch/ovtest/cases/agents/claude"
	agentcodex "code.byted.org/data-arch/ovtest/cases/agents/codex"
	agenthermes "code.byted.org/data-arch/ovtest/cases/agents/hermes"
	agentopenclaw "code.byted.org/data-arch/ovtest/cases/agents/openclaw"
	agentopencode "code.byted.org/data-arch/ovtest/cases/agents/opencode"
	agentpi "code.byted.org/data-arch/ovtest/cases/agents/pi"
	"code.byted.org/data-arch/ovtest/cases/experiment"
	openvikingcases "code.byted.org/data-arch/ovtest/cases/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

var allCases = buildCaseList()

func buildCaseList() []runner.Case {
	cases := openvikingcases.All()
	openclawCases := agentopenclaw.All()
	cases = append(cases, openclawCases...)
	if len(openclawCases) > 0 {
		legacy := openclawCases[0]
		legacy.ID = "openclaw-memory"
		cases = append(cases, legacy)
	}
	cases = append(cases, agenthermes.All()...)
	cases = append(cases, agentcodex.All()...)
	cases = append(cases, agentclaude.All()...)
	cases = append(cases, agentopencode.All()...)
	cases = append(cases, agentpi.All()...)
	cases = append(cases, experiment.All()...)
	return cases
}

var openVikingSuite = []string{
	"openviking-service-baseline",
	"ov-memory-update",
	"ov-experience-learning",
	"ov-negative-recall",
	"ov-retrieval-precision",
	"ov-memory-cjk",
	"openclaw-openviking-automatic-memory",
	"openclaw-openviking-tools",
	"hermes-openviking-automatic-memory",
	"hermes-openviking-tools",
	"codex-openviking-automatic-memory",
	"codex-openviking-mcp-tools",
	"claude-code-openviking-automatic-memory",
	"claude-code-openviking-mcp-tools",
	"opencode-openviking-automatic-memory",
	"opencode-openviking-mcp-tools",
	"openclaw-openviking-compaction",
	"claude-code-openviking-subagent-lifecycle",
	"pi-openviking-automatic-memory",
	"pi-openviking-tools",
	"pi-openviking-takeover-compaction",
}

var smokeCaseIDs = []string{
	"ov-memory-direct",
	"openclaw-memory",
}

var suiteOrder = []string{"openviking", "hermes-agent", "openclaw", "codex", "claude-code", "opencode", "pi", "smoke"}

var suites = map[string][]string{
	"openviking": openVikingSuite,
	"hermes-agent": {
		"hermes-openviking-automatic-memory", "hermes-openviking-tools",
	},
	"openclaw": {
		"openclaw-openviking-automatic-memory", "openclaw-openviking-tools", "openclaw-openviking-compaction",
	},
	"codex": {
		"codex-openviking-automatic-memory", "codex-openviking-mcp-tools",
	},
	"claude-code": {
		"claude-code-openviking-automatic-memory", "claude-code-openviking-mcp-tools", "claude-code-openviking-subagent-lifecycle",
	},
	"opencode": {
		"opencode-openviking-automatic-memory", "opencode-openviking-mcp-tools",
	},
	"pi": {
		"pi-openviking-automatic-memory", "pi-openviking-tools", "pi-openviking-takeover-compaction",
	},
	"smoke": smokeCaseIDs,
}

// All is the case registry (id -> Case), assembled from every case constructor.
var All = buildAll()

func buildAll() map[string]runner.Case {
	m := make(map[string]runner.Case, len(allCases))
	for _, c := range allCases {
		m[c.ID] = c
	}
	return m
}

// Names returns the case ids in registry order for `-list`/`all`.
func Names() []string {
	names := make([]string, len(allCases))
	for i, c := range allCases {
		names[i] = c.ID
	}
	return names
}

// DefaultNames returns the primary OpenViking release-gate suite used by `ovtest all`.
func DefaultNames() []string {
	ids, _ := Suite("openviking")
	return ids
}

// SmokeNames preserves the original two-case local smoke lane for quick
// development checks; it is intentionally separate from the release gate.
func SmokeNames() []string {
	return append([]string(nil), smokeCaseIDs...)
}

// Suite returns a copy of one named release lane. Harness suites contain only
// that integration; the OpenViking suite is the complete product release gate.
func Suite(name string) ([]string, bool) {
	ids, ok := suites[name]
	return append([]string(nil), ids...), ok
}

// SuiteNames returns stable human-facing suite order.
func SuiteNames() []string { return append([]string(nil), suiteOrder...) }
