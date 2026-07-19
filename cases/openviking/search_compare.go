package openviking

import (
	"fmt"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-search-compare: find vs search divergence on the same corpus. `ov find`
// (semantic retrieval) and `ov search` (experimental context-aware retrieval) run
// against the SAME seeded corpus and query; the judge compares whether both
// surface the target fact. find is the load-bearing gate; search runs SOFT (it may
// be flaky/empty) so the case reaches the judge for the comparison either way.
//
// DAG: user -> [add_mem x6] --(fan-in)--> wait -> [find, search] -> judge

var ovSearchCompareFacts = []string{
	"My name is Zayn and I maintain the OpenViking memory system.",
	"For systems programming I prefer Go over Python.",
	"My favorite coffee drink is a flat white.",
	"I usually work late and commit code after midnight.",
	"I run the OpenViking blog and write about agent memory.",
	"My main editor is Neovim with a heavily customised config.",
}

const ovSearchCompareGoal = "On one corpus, OpenViking's `find` and experimental `search` " +
	"retrievers should AGREE on a clearly-stated fact; the judge surfaces any divergence between them."

const ovSearchCompareReference = `Expected trace:
- "user": the preconfigured ovcli.conf identity is used; six facts are added,
  one of which clearly states the user prefers Go
  over Python for systems programming.
- "find": account-wide semantic retrieval for the systems-programming preference (the load-bearing
  retriever; it must surface the Go-over-Python fact).
- "search": the experimental context-aware retriever runs the SAME query (it may return fewer or
  different results — that divergence is the point; it is wired soft so the comparison still runs).
PASS only if BOTH the "find memories" block AND the "search memories" block surface the
Go-over-Python systems-programming preference. If ` + "`find`" + ` surfaces it but ` + "`search`" + ` does NOT (or the
search block is empty/absent), that is a retriever-divergence FAIL — say which retriever missed it.`

func ovSearchCompareCase() runner.Case {
	const query = "What does the user prefer for systems programming?"
	return runner.Case{
		ID:        "ov-search-compare",
		Goal:      ovSearchCompareGoal,
		Reference: ovSearchCompareReference,
		Build: func(b *dag.Builder) {
			user := runner.ConfiguredUser(b, "user")
			var adds []*dag.Node
			for i, fact := range ovSearchCompareFacts {
				adds = append(adds, b.Add(ovops.AddMemory, dag.Spec{
					Name: fmt.Sprintf("add_mem_%d", i+1),
					In:   dag.In{"user_key": user}, Config: dag.Cfg{"content": fact}}))
			}
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait", In: dag.In{"user_key": user, "after": runner.FanIn(b, adds...)},
				Config: dag.Cfg{"timeout": 55}})

			find := b.Add(ovops.Find, dag.Spec{
				Name: "find", In: dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{"query": query, "min_results": 1, "settle": 20, "retry": 8}})
			// search runs SOFT: experimental, may return nothing — we still reach the judge.
			search := b.Add(ovops.Search, dag.Spec{
				Name: "search", In: dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{"query": query, "min_results": 0, "gate": "soft", "settle": 20}})

			b.Add(checks.Judge, dag.Spec{Name: "judge",
				In:     dag.In{"memories": find, "search_memories": search},
				Config: dag.Cfg{"goal": ovSearchCompareGoal, "reference": ovSearchCompareReference}})
		},
	}
}
