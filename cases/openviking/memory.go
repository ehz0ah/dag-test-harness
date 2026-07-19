package openviking

import (
	"fmt"

	"code.byted.org/data-arch/ovtest/dag"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// ov-memory: the CROSS-SESSION memory pipeline, end to end. Personal facts are
// stated ONLY as conversation messages inside an ov-native session; `session
// commit` archives the transcript and extracts memories; then an ACCOUNT-WIDE
// `ov find` — run outside any session — retrieves the fact. Cross-session by
// construction: `ov find` has no session semantics, so the fact is findable only
// if extraction promoted it out of the session into durable user memory.
//
// DAG: user -> session_new -> msg_1..msg_6 -> commit -> wait
//      -> [ls, find] -> judge

type dialogueLine struct{ role, content string }

func commitMessages(b *dag.Builder, user *dag.Node, stem string, messages []map[string]any, minExtracted int, after ...dag.Input) *dag.Node {
	payload := b.Var(messages, stem+"_messages")
	sessionInputs := dag.In{"user_key": user}
	if len(after) > 0 {
		sessionInputs["after"] = after[0]
	}
	session := b.Add(ovops.SessionNew, dag.Spec{
		Name: "session_new_" + stem, In: sessionInputs,
	})
	added := b.Add(ovops.SessionAddMessages, dag.Spec{
		Name: "seed_" + stem,
		In: dag.In{
			"user_key": user, "session_id": session,
			"messages": payload, "after": session,
		},
	})
	return b.Add(ovops.SessionCommit, dag.Spec{
		Name: "commit_" + stem,
		In:   dag.In{"user_key": user, "session_id": session, "after": added},
		Config: dag.Cfg{
			"min_extracted": minExtracted, "settle": 3, "retry": 60,
			"cleanup_added_memories": true,
		},
	})
}

var ovMemoryDialogue = []dialogueLine{
	{"user", "My name is Zayn and I maintain the OpenViking memory system."},
	{"assistant", "Nice to meet you, Zayn — noted that you maintain OpenViking."},
	{"user", "For systems programming I prefer Go over Python."},
	{"assistant", "Got it: Go over Python for systems programming."},
	{"user", "My favorite coffee drink is a flat white."},
	{"assistant", "Flat white — noted."},
}

const ovMemoryGoal = "Facts stated inside an OpenViking conversation session become durable " +
	"user memories after `ov session commit`, retrievable OUTSIDE the session via " +
	"account-wide semantic find for the configured user."

const ovMemoryReference = `Expected trace:
- "user": the preconfigured ovcli.conf identity is used.
- "session_new": an ov-native conversation session is created (a session_id is returned).
- "msg_1..msg_6": three personal facts are stated ONLY as user messages in that session —
  (1) the user's name is Zayn and they maintain OpenViking;
  (2) for systems programming the user prefers Go over Python;
  (3) the user's favorite coffee is a flat white —
  each acknowledged by an assistant message, message_count incrementing 1..6.
- "commit": the session is committed — messages archived, asynchronous memory extraction
  accepted, and the server subsequently reports at least one memory extracted from the transcript.
- "ls": lists the account's viking scopes.
- "find": the query "What does the user prefer for systems programming?" is an ACCOUNT-WIDE
  semantic search run after commit. ` + "`ov find`" + ` has no session parameter — that is precisely the
  property under test: the fact must have been promoted out of the session into the durable user
  scope to be found at all. It must return at least one memory under .../memories/preferences/...
  whose abstract states the user prefers Go over Python. Extraction also writes procedural records
  under .../memories/trajectories/ and .../memories/experiences/ — those do NOT satisfy the pass
  condition.
PASS only if "find" surfaces a preference memory stating the Go-over-Python preference, given that
the fact entered the system exclusively through session messages (never via direct add-memory).`

func ovMemoryCase() runner.Case {
	const query = "What does the user prefer for systems programming?"
	return runner.Case{
		ID:        "ov-memory",
		Goal:      ovMemoryGoal,
		Reference: ovMemoryReference,
		Build: func(b *dag.Builder) {
			user := runner.ConfiguredUser(b, "user")
			session := b.Add(ovops.SessionNew, dag.Spec{
				Name: "session_new", In: dag.In{"user_key": user}})

			// LINEAR chain: transcript order is semantic, and each node's expect_count
			// gate (message_count == position) is only deterministic sequentially.
			var prev dag.Input = session
			for i, line := range ovMemoryDialogue {
				prev = b.Add(ovops.SessionAddMessage, dag.Spec{
					Name: fmt.Sprintf("msg_%d", i+1),
					In:   dag.In{"user_key": user, "session_id": session, "after": prev},
					Config: dag.Cfg{"role": line.role, "content": line.content,
						"expect_count": i + 1}})
			}

			commit := b.Add(ovops.SessionCommit, dag.Spec{
				Name: "commit", In: dag.In{"user_key": user, "session_id": session, "after": prev},
				Config: dag.Cfg{"settle": 5, "retry": 12}})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait", In: dag.In{"user_key": user, "after": commit},
				Config: dag.Cfg{"timeout": 55}})

			runner.RetrieveAndJudge(b, user, wait, query, ovMemoryGoal, ovMemoryReference)
		},
	}
}
