package openviking

import (
	"fmt"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-retrieval-precision: retrieval precision under same-domain distractors. Seed
// six coffee facts that differ only by CONTEXT (home / work / weekend / ...), then
// ask specifically about coffee AT WORK. Retrieval readiness stays structural (>= 1
// memory indexed); precision — did the work-espresso fact surface — is the judge's
// semantic call (same-domain scores overlap and cannot rank them).
//
// DAG: user -> one six-fact session commit -> corpus readiness
//      -> [ls, context-specific find] -> judge

func ovPrecisionFacts(schedule string) []string {
	return []string{
		fmt.Sprintf("The coffee schedule named %s specifies a flat white at home in the morning.", schedule),
		fmt.Sprintf("The coffee schedule named %s specifies a double espresso at the office during the workday.", schedule),
		fmt.Sprintf("The coffee schedule named %s specifies cold brew at home on weekends.", schedule),
		fmt.Sprintf("The coffee schedule named %s specifies decaf after dinner.", schedule),
		fmt.Sprintf("The coffee schedule named %s specifies plain black coffee before a workout.", schedule),
		fmt.Sprintf("The coffee schedule named %s specifies a cappuccino while traveling for conferences.", schedule),
	}
}

const ovPrecisionGoal = "OpenViking retrieves the CONTEXT-correct memory among several same-domain " +
	"distractors: asked about coffee at work, it surfaces the office-espresso fact, not the home / " +
	"weekend / travel ones."

const ovPrecisionReference = `Expected trace:
- "user": the preconfigured ovcli.conf identity is used; six facts from one uniquely named coffee schedule are added,
  each tying a DIFFERENT context to a
  different drink: home = flat white, WORK/office = double espresso, weekends = cold brew, after
  dinner = decaf, pre-workout = black coffee, travel = cappuccino.
- "find": the query asks specifically what coffee the user drinks AT WORK; at least one coffee
  memory is indexed and returned.
PASS only if the retrieved memories let the work coffee be identified as the OFFICE DOUBLE ESPRESSO
(the work-context fact). A result that surfaces only non-work coffees (flat white / cold brew /
cappuccino / decaf / black) as the answer, or that picks the wrong context's drink as "at work", is
a precision FAIL.`

func ovRetrievalPrecisionCase() runner.Case {
	return runner.Case{
		ID:        "ov-retrieval-precision",
		Goal:      ovPrecisionGoal,
		Reference: ovPrecisionReference,
		Build: func(b *dag.Builder) {
			schedule := "Atlas Finch " + strings.ToUpper(nonce(4))
			query := "According to the " + schedule + " coffee schedule, what coffee is specified for the office workday?"
			user := runner.ConfiguredUser(b, "user")
			messages := make([]map[string]any, 0, 2*len(ovPrecisionFacts(schedule)))
			for _, fact := range ovPrecisionFacts(schedule) {
				messages = append(messages,
					map[string]any{"role": "user", "content": fact},
					map[string]any{"role": "assistant", "content": "Acknowledged."},
				)
			}
			commit := commitMessages(b, user, "schedule", messages, 1, user)
			ready := b.Add(ovops.Find, dag.Spec{
				Name: "find_seeded_corpus", In: dag.In{"user_key": user, "after": commit},
				Config: dag.Cfg{
					"query": schedule, "expect": []string{strings.ToLower(schedule)}, "min_results": 1,
					"settle": 3, "retry": 30, "cleanup_kind": "memory",
					"cleanup_marker": strings.ToLower(schedule),
				},
			})
			ls := b.Add(ovops.List, dag.Spec{Name: "ls", In: dag.In{"user_key": user, "after": ready}})
			find := b.Add(ovops.Find, dag.Spec{
				Name: "find", In: dag.In{"user_key": user, "after": ready},
				Config: dag.Cfg{
					"query": query, "min_results": 1,
					"expect": []string{strings.ToLower(schedule), "double espresso"},
					"settle": 3, "retry": 20,
				},
			})

			b.Add(checks.Judge, dag.Spec{Name: "judge",
				In:     dag.In{"created": user, "memories": find, "entries": ls},
				Config: dag.Cfg{"goal": ovPrecisionGoal, "reference": ovPrecisionReference}})
		},
	}
}
