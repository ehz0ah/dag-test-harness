package cases

import (
	"reflect"
	"testing"

	"code.byted.org/data-arch/ovtest/dag"
)

// Offline DAG smoke: every real case must build, construct an executor (which
// rejects cycles + dangling refs), and carry exactly one terminal gate.
// This catches a mis-authored case (undeclared input, cycle, non-terminal gate)
// in milliseconds, with no live calls.

func TestAllCasesBuildAcyclicWithTerminalJudge(t *testing.T) {
	if len(All) != len(Names()) {
		t.Fatalf("All has %d cases, Names() lists %d", len(All), len(Names()))
	}
	for _, id := range Names() {
		c, ok := All[id]
		if !ok {
			t.Errorf("Names() lists %q but it is not in All", id)
			continue
		}
		if c.ID != id {
			t.Errorf("registry key %q != case ID %q", id, c.ID)
		}

		b := dag.New()
		c.Build(b)
		workflow, err := b.Build()
		if err != nil {
			t.Errorf("%s: build: %v", id, err)
			continue
		}
		ex, err := dag.NewExecutor(workflow)
		if err != nil {
			t.Errorf("%s: executor (cycle / dangling ref?): %v", id, err)
			continue
		}

		judges := 0
		succ := ex.Successors()
		for _, n := range workflow.Nodes() {
			if workflow.Terminal(n) {
				judges++
				if len(succ[n]) > 0 {
					t.Errorf("%s: judge %q must be terminal, has successors %v", id, n, succ[n])
				}
			}
		}
		if judges != 1 {
			t.Errorf("%s: want exactly one terminal gate, got %d", id, judges)
		}
	}
}

func TestDefaultNamesAreRegistered(t *testing.T) {
	for _, id := range DefaultNames() {
		if _, ok := All[id]; !ok {
			t.Fatalf("default case %q is not registered", id)
		}
	}
}

func TestSmokeNamesPreserveOriginalQuickLane(t *testing.T) {
	want := []string{"ov-memory-direct", "openclaw-memory"}
	if got := SmokeNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SmokeNames() = %v, want %v", got, want)
	}
	for _, id := range want {
		if _, ok := All[id]; !ok {
			t.Fatalf("smoke case %q is not registered", id)
		}
	}
}

func TestDefaultNamesExcludeExperimentCases(t *testing.T) {
	for _, id := range DefaultNames() {
		if len(id) >= len("experiment-") && id[:len("experiment-")] == "experiment-" {
			t.Fatalf("experiment case %q must be explicit, not part of ovtest all", id)
		}
	}
}

func TestDefaultNamesAreExactPrimaryReleaseGate(t *testing.T) {
	want := []string{
		"openviking-service-baseline",
		"ov-memory-update", "ov-experience-learning", "ov-negative-recall",
		"ov-retrieval-precision", "ov-memory-cjk",
		"openclaw-openviking-automatic-memory", "openclaw-openviking-tools",
		"hermes-openviking-automatic-memory", "hermes-openviking-tools",
		"codex-openviking-automatic-memory", "codex-openviking-mcp-tools",
		"claude-code-openviking-automatic-memory", "claude-code-openviking-mcp-tools",
		"opencode-openviking-automatic-memory", "opencode-openviking-mcp-tools",
		"openclaw-openviking-compaction", "claude-code-openviking-subagent-lifecycle",
		"pi-openviking-automatic-memory", "pi-openviking-tools", "pi-openviking-takeover-compaction",
	}
	got := DefaultNames()
	if len(got) != len(want) {
		t.Fatalf("primary release gate has %d cases, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("primary release gate[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNamedSuitesAreRegisteredAndScoped(t *testing.T) {
	wantSizes := map[string]int{
		"openviking": 21, "hermes-agent": 2, "openclaw": 3, "codex": 2,
		"claude-code": 3, "opencode": 2, "pi": 3, "smoke": 2,
	}
	for _, name := range SuiteNames() {
		ids, ok := Suite(name)
		if !ok || len(ids) != wantSizes[name] {
			t.Fatalf("suite %q = %v, ok=%v", name, ids, ok)
		}
		for _, id := range ids {
			if _, registered := All[id]; !registered {
				t.Fatalf("suite %q contains unregistered case %q", name, id)
			}
			if name != "openviking" && name != "smoke" && caseSuitePrefix(name, id) == false {
				t.Fatalf("suite %q leaked unrelated case %q", name, id)
			}
		}
	}
	if _, ok := Suite("unknown"); ok {
		t.Fatal("unknown suite was accepted")
	}
}

func caseSuitePrefix(suite, id string) bool {
	prefix := map[string]string{
		"hermes-agent": "hermes-", "openclaw": "openclaw-", "codex": "codex-",
		"claude-code": "claude-code-", "opencode": "opencode-", "pi": "pi-",
	}[suite]
	return prefix != "" && len(id) >= len(prefix) && id[:len(prefix)] == prefix
}

func TestCodexCasesAreRegistered(t *testing.T) {
	for _, id := range []string{
		"codex-openviking-automatic-memory",
		"codex-openviking-mcp-tools",
	} {
		if _, ok := All[id]; !ok {
			t.Fatalf("codex case %q is not registered", id)
		}
	}
}
