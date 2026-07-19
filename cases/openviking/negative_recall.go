package openviking

import (
	"fmt"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-negative-recall: hallucination / negative-recall check. Seed a few REAL facts,
// then query a fact that was NEVER stated. The system must NOT have a memory
// answering it: `ov find` returns nothing relevant (min_results:0) AND — the
// deterministic leak gate — no returned abstract may contain the never-stated
// nonce (`forbid`). The judge then confirms no memory asserts the fabricated
// attribute. Confident hallucination is the #1 agent-memory failure.
//
// DAG: user -> session/messages -> commit-task -> positive readiness
//      -> negative find(forbid) -> judge

func ovNegativeRecallFacts(marker string) []string {
	return []string{
		fmt.Sprintf("Profile %s belongs to Zayn, who maintains OpenViking.", marker),
		fmt.Sprintf("Profile %s prefers Go over Python for systems programming.", marker),
		fmt.Sprintf("Profile %s prefers a flat white coffee.", marker),
	}
}

const ovNegativeRecallGoal = "OpenViking must not fabricate a memory for a fact the user never " +
	"stated — a query about an unknown attribute returns no answer, not a hallucinated one."

func ovNegativeRecallReference(badge string) string {
	return fmt.Sprintf(`Expected trace:
- "user": the preconfigured ovcli.conf identity is used; three REAL facts are added
  (name, language preference, coffee).
- "find": the query asks for the user's security badge number — a fact NEVER stated anywhere.
  `+"`ov find`"+` is account-wide; min_results:0, so returning zero relevant memories is acceptable, and
  the deterministic forbid gate has already verified that NO returned abstract contains the
  never-stated badge token "%s".
PASS only if NO returned memory asserts a security badge number for the user (none should exist —
it was never stated). A memory that invents a badge number — in particular one containing
"%s" — is a hallucinated/fabricated memory and is a FAIL. Returning only the unrelated real
facts (name / language / coffee), or nothing at all, is a PASS.`, badge, badge)
}

func ovNegativeRecallCase() runner.Case {
	return runner.Case{
		ID:        "ov-negative-recall",
		Goal:      ovNegativeRecallGoal,
		Reference: ovNegativeRecallReference("z-<nonce>"),
		Build: func(b *dag.Builder) {
			// the badge number is a per-build nonce that is NEVER seeded — so it cannot
			// appear in any abstract unless the system fabricated it.
			marker := "OVNEG-" + strings.ToUpper(nonce(6))
			badge := "z-" + nonce(3)
			user := runner.ConfiguredUser(b, "user")

			messages := make([]map[string]any, 0, 6)
			for _, fact := range ovNegativeRecallFacts(marker) {
				messages = append(messages,
					map[string]any{"role": "user", "content": fact},
					map[string]any{"role": "assistant", "content": "Acknowledged."},
				)
			}
			commit := commitMessages(b, user, "known_facts", messages, 1)
			ready := b.Add(ovops.Find, dag.Spec{
				Name: "find_known_fact", In: dag.In{"user_key": user, "after": commit},
				Config: dag.Cfg{
					"query": marker, "expect": []string{strings.ToLower(marker)}, "min_results": 1,
					"settle": 3, "retry": 30, "cleanup_kind": "memory",
					"cleanup_marker": strings.ToLower(marker),
				},
			})

			// Prove the corpus is indexed before accepting an empty negative result.
			find := b.Add(ovops.Find, dag.Spec{
				Name: "find", In: dag.In{"user_key": user, "after": ready},
				Config: dag.Cfg{"query": "What is the security badge number for profile " + marker + "?",
					"min_results": 0, "forbid": []string{badge}}})

			b.Add(checks.Judge, dag.Spec{Name: "judge", In: dag.In{"memories": find},
				Config: dag.Cfg{"goal": ovNegativeRecallGoal,
					"reference": ovNegativeRecallReference(badge)}})
		},
	}
}
