package openclaw

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	openclawadapter "code.byted.org/data-arch/ovtest/adapters/openclaw"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

var openclawMemoryTools = []string{"memory_store", "memory_recall", "memory_forget", "ov_search", "ov_read", "ov_multi_read", "ov_list", "add_resource"}

// openclaw-memory: the OpenClaw <-> OpenViking round-trip through the local
// embedded OpenClaw agent. Chat a personal fact into openclaw (whose plugin
// auto-captures and extracts the turn into OpenViking), verify the extracted
// memory, then ask openclaw to recall it in a separate session — and judge
// that the recall surfaces the fact. Cross-session by construction.
//
// DAG: user -> chat_capture -> verify_captured_session
//     -> find_extracted_alias + find_extracted_color -> chat_recall -> judge
//
// Each chat passes the OpenViking URL/API key to the already-configured local
// OpenClaw plugin, avoiding ambient OpenClaw gateway pairing. A per-BUILD nonce
// makes a green recall provably this run's (safe under --repeat).

const openclawAccent = "marigold"

const openclawGoal = "OpenClaw, via its OpenViking context-engine plugin, persists a chatted fact " +
	"into OpenViking and can recall it in a separate session."

func openclawReference(alias string) string {
	return fmt.Sprintf(`Expected trace:
- "chat_capture": the user tells openclaw their stable project alias is %s and their preferred theme color is %s; the local OpenClaw agent acknowledges, using the preconfigured OpenViking user.
- "find_extracted_alias" and "find_extracted_color": OpenViking contains independently retrievable preference memories for alias "%s" and theme color %s — proof the hook captured and extracted the chat.
- "chat_recall": in a separate openclaw session (no shared conversation history) the user asks for their project alias and preferred theme color; openclaw's reply states them.
PASS only if the "chat_recall" reply actually states BOTH the alias "%s" and theme color %s (the memory round-tripped and was fetched back). A reply that says it doesn't know, or names a different alias or color, is a FAIL.`,
		alias, openclawAccent, alias, openclawAccent, alias, openclawAccent)
}

func openclawCapturePrompt(alias string) string {
	return fmt.Sprintf("This is an ordinary conversation. Do not call tools or memory functions. "+
		"These are stable personal preferences I want retained for future chats: my project alias is %s, and my preferred theme color is %s. "+
		"Confirm both values in one short sentence.", alias, openclawAccent)
}

func openclawRecallPrompt() string {
	return "Using only context supplied automatically, what are my project alias and preferred theme color from the earlier conversation? " +
		"Do not call tools or memory functions. Answer with only the alias and color; if you don't know, say you don't know."
}

func memoryCase() runner.Case {
	return runner.Case{
		ID:        "openclaw-openviking-automatic-memory",
		Goal:      openclawGoal,
		Reference: openclawReference("Basalt-<nonce>"),
		Build: func(b *dag.Builder) {
			// Unique per BUILD -> safe under --repeat: a recall hit can only come from
			// THIS run's capture.
			n := nonce(12)
			alias := "Basalt-" + n
			captureSession := "ovclaw-cap-" + n
			recallSession := "ovclaw-recall-" + n
			stateDir := filepath.Join(runner.StateDir(), "openclaw", "automatic-memory-"+n)
			fact := openclawCapturePrompt(alias)
			recallQ := openclawRecallPrompt()

			user := runner.ConfiguredUser(b, "user")

			// state the fact -> the plugin afterTurn captures it into OpenViking.
			capture := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_capture", In: dag.In{"user_key": user},
				Config: dag.Cfg{"message": fact, "session_id": captureSession, "state_dir": stateDir, "auto_capture": true, "auto_recall": false}})
			captureNoTools := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"transcript": capture.Out("transcript"), "after": capture},
				Config: dag.Cfg{"forbid_tools": openclawMemoryTools},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_captured_session",
				In:   dag.In{"user_key": user, "session_id": capture.Out("ov_session_id"), "after": captureNoTools},
				Config: dag.Cfg{
					"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true,
				}})

			// OpenClaw may archive the active transcript immediately after capture,
			// leaving messages.jsonl empty. The extracted memory is the stable,
			// product-relevant proof that the automatic lifecycle path completed.
			aliasFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_alias", In: dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": alias, "expect": []string{strings.ToLower(alias)},
					"uri": "viking://user/memories", "min_results": 1, "settle": 10, "retry": 18,
				},
			})
			colorFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_color", In: dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": openclawAccent, "expect": []string{openclawAccent},
					"uri": "viking://user/memories", "min_results": 1, "settle": 10, "retry": 18,
				},
			})

			// fetch it back in a fresh session
			recall := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_recall", In: dag.In{"user_key": user, "after": b.Merge(aliasFound, colorFound)},
				Config: dag.Cfg{"message": recallQ + " Do not use tools.", "session_id": recallSession, "state_dir": stateDir, "auto_capture": false, "auto_recall": true}})
			recallNoTools := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"transcript": recall.Out("transcript"), "after": recall},
				Config: dag.Cfg{"forbid_tools": openclawMemoryTools},
			})

			// terminal judge: scores the capture transcript + the recall reply only
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": recall.Out("reply"), "after": b.Merge(aliasFound, colorFound, recallNoTools)},
				Config: dag.Cfg{
					"expect": []string{strings.ToLower(alias), openclawAccent},
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
