package openviking

import (
	"fmt"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-memory-direct: direct `add-memory` writes + semantic retrieval. The cheap
// secondary case — three facts added via `ov add-memory` (bypassing the session
// pipeline) for the configured user, then verified retrievable. It uses the same
// retrieval/judge shape as ov-memory via a different ingestion path, so the two
// form a differential (extraction present vs absent).
//
// DAG: user -> add_mem_1 -> add_mem_2 -> add_mem_3 -> wait -> [ls, find] -> judge

type ovMemoryDirectFact struct {
	content string
}

func ovMemoryDirectFacts() []ovMemoryDirectFact {
	return []ovMemoryDirectFact{
		{"My name is Zayn and I maintain the OpenViking memory system."},
		{"For systems programming I prefer Go over Python."},
		{"My favorite coffee drink is a flat white."},
	}
}

const ovMemoryDirectGoal = "OpenViking stores personal facts added directly via add-memory and " +
	"retrieves them semantically for the configured user."

const ovMemoryDirectReference = `Expected trace:
- "user": the preconfigured ovcli.conf identity is used.
- "add_mem_1..3": three personal facts added directly via ` + "`ov add-memory`" + `, each ok:
  (1) the user's name is Zayn and they maintain OpenViking;
  (2) for systems programming the user prefers Go over Python;
  (3) the user's favorite coffee is a flat white.
- "ls": lists the account's viking scopes (session/user/resources).
- "find": directly asks whether the user prefers Go or Python for systems programming and returns
  at least one memory whose abstract carries the Go-over-Python fact.
PASS only if "find" actually surfaces the Go-over-Python preference memory. A generic memory
directory abstract, an unrelated high-score item, or a path-only match is not enough.`

func ovMemoryDirectCase() runner.Case {
	return runner.Case{
		ID:        "ov-memory-direct",
		Goal:      ovMemoryDirectGoal,
		Reference: ovMemoryDirectReference,
		Build: func(b *dag.Builder) {
			query := "Does the user prefer Go or Python for systems programming?"
			user := runner.ConfiguredUser(b, "user")

			var prev dag.Input = user
			for i, fact := range ovMemoryDirectFacts() {
				prev = b.Add(ovops.AddMemory, dag.Spec{
					Name: fmt.Sprintf("add_mem_%d", i+1),
					In:   dag.In{"user_key": user, "after": prev},
					Config: dag.Cfg{
						"content": fact.content,
					}})
			}

			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait", In: dag.In{"user_key": user, "after": prev},
				Config: dag.Cfg{"timeout": 55}})

			ls := b.Add(ovops.List, dag.Spec{Name: "ls",
				In: dag.In{"user_key": user, "after": wait}})
			find := b.Add(ovops.Find, dag.Spec{Name: "find",
				In: dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{"query": query, "min_results": 1,
					"expect": []string{"go", "python"}, "settle": 20, "retry": 8}})
			b.Add(checks.Judge, dag.Spec{Name: "judge",
				In:     dag.In{"created": user, "memories": find.Out("relevant"), "entries": ls},
				Config: dag.Cfg{"goal": ovMemoryDirectGoal, "reference": ovMemoryDirectReference}})
		},
	}
}
