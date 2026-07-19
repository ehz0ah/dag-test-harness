package openviking

import (
	"fmt"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-forget-ghost: forget / rm semantics — no ghost-resurfacing after delete. The
// classic memory-system bug: `rm` removes the resource but the vector index keeps
// surfacing it. Seed one distinctive fact, confirm it is retrievable, remove it
// (ov_rm picks the URI from the wired find result by abstract), then find again
// with INVERTED readiness (expect_gone) — the poll succeeds only once the memory is
// GONE from the index, and HARD-fails if the ghost is still resurfacing. The judge
// then catches any SEMANTIC residue.
//
// DAG: user -> seed -> wait -> find_before -> rm -> find_after -> judge

const ovForgetGoal = "A memory removed with `ov rm` stops being retrievable — `find` no longer " +
	"surfaces it (the vector index does not ghost-resurface a deleted resource)."

func ovForgetReference(token string) string {
	return fmt.Sprintf(`Expected trace:
- "user": the preconfigured ovcli.conf identity is used; one distinctive fact is added —
  the user's security clearance is "%s".
- "find_before" (gate-guaranteed): an account-wide query returned the memory stating "%s".
- "rm": every returned memory containing that token was removed (URIs resolved from the find result by abstract).
- "find_after": the SAME query was re-run with inverted readiness — it succeeds only once NO memory
  whose abstract contains "%s" remains (the resource is gone from the index, not resurfacing).
PASS only if the evidence shows NO memory still asserts the "%s" clearance after removal — and
in particular no OTHER memory references that clearance indirectly (no semantic residue). Any memory
that still surfaces the deleted clearance is a ghost-resurfacing FAIL.`, token, token, token, token)
}

func ovForgetGhostCase() runner.Case {
	const clearanceQ = "What is the user's security clearance level?"
	return runner.Case{
		ID:        "ov-forget-ghost",
		Goal:      ovForgetGoal,
		Reference: ovForgetReference("Delta-<nonce>"),
		Build: func(b *dag.Builder) {
			token := "Delta-" + nonce(2)
			fact := "My security clearance level is " + token + "."
			user := runner.ConfiguredUser(b, "user")

			seed := b.Add(ovops.AddMemory, dag.Spec{
				Name: "seed", In: dag.In{"user_key": user}, Config: dag.Cfg{"content": fact}})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait", In: dag.In{"user_key": user, "after": seed},
				Config: dag.Cfg{"timeout": 55}})

			findBefore := b.Add(ovops.Find, dag.Spec{
				Name: "find_before", In: dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{"query": clearanceQ, "expect": []string{strings.ToLower(token)},
					"min_results": 1, "settle": 20, "retry": 8}})
			// remove the matched memory (URI resolved inside the op from the abstract)
			rm := b.Add(ovops.Remove, dag.Spec{
				Name: "rm", In: dag.In{"user_key": user, "memories": findBefore},
				Config: dag.Cfg{"abstract_filter": strings.ToLower(token), "all_matches": true}})
			// inverted readiness: poll until the clearance memory is GONE from the index.
			findAfter := b.Add(ovops.Find, dag.Spec{
				Name: "find_after", In: dag.In{"user_key": user, "after": rm},
				Config: dag.Cfg{"query": clearanceQ, "expect": []string{strings.ToLower(token)},
					"expect_gone": true, "settle": 10, "retry": 8}})

			// judge sees the AFTER state (BEFORE presence is already gate-guaranteed).
			b.Add(checks.Judge, dag.Spec{Name: "judge",
				In:     dag.In{"created": user, "memories": findAfter},
				Config: dag.Cfg{"goal": ovForgetGoal, "reference": ovForgetReference(token)}})
		},
	}
}
