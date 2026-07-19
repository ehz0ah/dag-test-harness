package openviking

import (
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-memory-cjk: CJK (non-Latin) content round-trip. Seed a Chinese preference
// sentence (with the latin token 'Rust' inside) and query in Chinese. The
// project name is the subject of the fact itself, so normalized memories retain a safe
// cleanup scope without requiring preservation of a synthetic test prefix. The
// judge checks the CJK content round-tripped INTACT — readable
// Chinese, no mojibake — and conveys the stated preference.
//
// DAG: user -> session(cjk) -> commit-task -> [ls, find] -> judge

// "What is the project's core systems-programming language?"
const ovCJKQuery = "的核心系统编程语言是什么？"

// "The project's core systems-programming language is Rust because it balances memory
// safety and high performance."
const ovCJKFact = "的核心系统编程语言是 Rust，因为它兼顾内存安全和高性能。"

const ovCJKGoal = "OpenViking round-trips non-Latin (CJK) content intact: a Chinese preference is " +
	"stored, a Chinese query retrieves it, and the abstract stays readable Chinese — no mojibake or " +
	"truncation of meaning."

const ovCJKReference = `Expected trace:
- "user": the preconfigured ovcli.conf identity is used; one Chinese project memory is added
  stating the project's core systems-programming language is Rust (核心系统编程语言是 Rust，因为内存安全和高性能).
- "find": a CHINESE-language query about the project's systems-programming language returns
  at least one relevant memory (readiness keyed on the latin token "rust" appearing in the abstract).
PASS only if the retrieved abstract is intact, readable Chinese (NO mojibake / replacement
characters / garbled bytes) AND conveys that the project uses Rust for systems programming. Garbled
or empty CJK, or an abstract that loses the Rust preference, is a FAIL.`

func ovMemoryCJKCase() runner.Case {
	return runner.Case{
		ID:        "ov-memory-cjk",
		Goal:      ovCJKGoal,
		Reference: ovCJKReference,
		Build: func(b *dag.Builder) {
			marker := strings.ToUpper(nonce(4))
			project := "青鸾计划 " + marker
			fact := "项目 " + project + ovCJKFact
			query := "项目 " + project + ovCJKQuery
			user := runner.ConfiguredUser(b, "user")
			commit := commitMessages(b, user, "cjk", []map[string]any{
				{"role": "user", "content": fact},
				{"role": "assistant", "content": "已记录。"},
			}, 1)

			ls := b.Add(ovops.List, dag.Spec{Name: "ls", In: dag.In{"user_key": user, "after": commit}})
			// OpenViking may normalize the identifier into a separate key-fact line,
			// so readiness requires all terms without assuming they stay adjacent.
			find := b.Add(ovops.Find, dag.Spec{
				Name: "find", In: dag.In{"user_key": user, "after": commit},
				Config: dag.Cfg{
					"query": query, "min_results": 1,
					"expect": []string{"青鸾计划", strings.ToLower(marker), "rust"}, "settle": 3, "retry": 30,
				},
			})

			b.Add(checks.Judge, dag.Spec{Name: "judge",
				In:     dag.In{"created": user, "memories": find, "entries": ls},
				Config: dag.Cfg{"goal": ovCJKGoal, "reference": ovCJKReference}})
		},
	}
}
