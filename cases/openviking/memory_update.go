package openviking

import (
	"fmt"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-memory-update: memory update / conflict — which fact wins? The user states a
// preference, then RETRACTS and replaces it inside one session. After commit +
// extraction, account-wide find must surface the UPDATED preference, not the stale
// one. "Which fact wins" is purely semantic -> the judge decides; the find gate
// stays structural (a /preferences/ memory exists).

func ovMemoryUpdateDialogue(marker string) []dialogueLine {
	return []dialogueLine{
		{"user", fmt.Sprintf("For project %s systems programming I prefer Go over Python.", marker)},
		{"assistant", fmt.Sprintf("Got it — project %s uses Go over Python.", marker)},
		{"user", fmt.Sprintf("Actually, scratch that — project %s now uses Rust over Go for systems programming.", marker)},
		{"assistant", fmt.Sprintf("Understood — project %s now prefers Rust over Go.", marker)},
	}
}

const ovMemoryUpdateGoal = "When a user updates a stated preference within a session, OpenViking's " +
	"extracted memory reflects the NEW preference, and account-wide recall surfaces the current one " +
	"— not the superseded fact."

const ovMemoryUpdateReference = `Expected trace:
- "user": the preconfigured ovcli.conf identity is used; an ov-native session is created.
- "msg_1..msg_4": the user first says they prefer Go over Python, then RETRACTS and says they have
  switched to Rust over Go; each is acknowledged (message_count incrementing 1..4).
- "commit": the session is committed and at least one memory is extracted from the transcript.
- "find": an account-wide query for the user's CURRENT systems-programming language preference
  surfaces a preference memory under .../memories/preferences/...
PASS only if the surfaced preference reflects the UPDATED choice — Rust (the latest stated
preference), or Rust shown as superseding Go. A recall that surfaces ONLY Go (the retracted
preference) and presents it as current is a FAIL. Which fact wins is the semantic question under
test — do not accept the stale Go-only answer.`

func ovMemoryUpdateCase() runner.Case {
	return runner.Case{
		ID:        "ov-memory-update",
		Goal:      ovMemoryUpdateGoal,
		Reference: ovMemoryUpdateReference,
		Build: func(b *dag.Builder) {
			marker := "OVUPDATE-" + strings.ToUpper(nonce(6))
			query := "What language is currently preferred for project " + marker + " systems programming?"
			user := runner.ConfiguredUser(b, "user")
			session := b.Add(ovops.SessionNew, dag.Spec{
				Name: "session_new", In: dag.In{"user_key": user}})

			var prev dag.Input = session
			for i, line := range ovMemoryUpdateDialogue(marker) {
				prev = b.Add(ovops.SessionAddMessage, dag.Spec{
					Name: fmt.Sprintf("msg_%d", i+1),
					In:   dag.In{"user_key": user, "session_id": session, "after": prev},
					Config: dag.Cfg{"role": line.role, "content": line.content,
						"expect_count": i + 1}})
			}

			commit := b.Add(ovops.SessionCommit, dag.Spec{
				Name: "commit", In: dag.In{"user_key": user, "session_id": session, "after": prev},
				Config: dag.Cfg{"settle": 3, "retry": 60, "cleanup_added_memories": true}})
			ls := b.Add(ovops.List, dag.Spec{Name: "ls", In: dag.In{"user_key": user, "after": commit}})
			find := b.Add(ovops.Find, dag.Spec{
				Name: "find", In: dag.In{"user_key": user, "after": commit},
				Config: dag.Cfg{
					"query": query, "uri": "viking://user/memories", "min_results": 1,
					"expect": []string{strings.ToLower(marker), "rust"}, "settle": 3, "retry": 30,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(marker),
				},
			})
			b.Add(checks.Judge, dag.Spec{
				Name: "judge", In: dag.In{"created": user, "memories": find.Out("relevant"), "entries": ls},
				Config: dag.Cfg{"goal": ovMemoryUpdateGoal, "reference": ovMemoryUpdateReference},
			})
		},
	}
}
