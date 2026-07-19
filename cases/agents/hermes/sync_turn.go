package hermes

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	hermesadapter "code.byted.org/data-arch/ovtest/adapters/hermes"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

// hermes-openviking-sync-turn: exercise Hermes' normal completed-turn sync path.
// The capture prompt is a plain story, not an explicit remember/store request.
// Hermes exits after the single-query chat; its OpenViking plugin should commit
// the session, OpenViking should extract the story fact, and a fresh Hermes home
// should recall it through OpenViking context rather than local conversation.

const hermesStoryColor = "slate green"

const hermesGoal = "Hermes records a normal story turn into OpenViking via sync_turn/session-end commit and recalls it from a fresh Hermes session."

func hermesCapturePrompt(codename string) string {
	return fmt.Sprintf("This is a standalone operational update and no clarification is needed. "+
		"Mira checked the launch board one last time before sunrise. The project codename was %s, "+
		"and the handoff color for the standby crew was %s. Respond with exactly one calm acknowledgment sentence. "+
		"Do not call tools or ask questions.", codename, hermesStoryColor)
}

func hermesAutomaticForbiddenTools() []string {
	return append([]string{"clarify"}, hermesOpenVikingToolNames...)
}

func hermesReference(codename string) string {
	return fmt.Sprintf(`Expected trace:
- "chat_capture": Hermes receives a normal story prompt saying Mira's project codename is %s and the handoff color is %s. The prompt does not ask Hermes to remember or store anything.
- "wait_for_commit": ovtest only observes OpenViking session state. It must see Hermes' session already committed and at least one memory extracted; it must not call "ov session commit".
- "find_story_memory": OpenViking retrieval finds a memory containing both "%s" and "%s".
- "chat_recall": a fresh Hermes home asks about Mira's story and the reply states both values.
PASS only if the recall answer states BOTH the exact codename "%s" and handoff color "%s". A reply that says it does not know, guesses a different codename, or only passes because the test wrote memory directly is a FAIL.`,
		codename, hermesStoryColor, codename, hermesStoryColor, codename, hermesStoryColor)
}

func syncTurnCase() runner.Case {
	return runner.Case{
		ID:        "hermes-openviking-automatic-memory",
		Goal:      hermesGoal,
		Reference: hermesReference("VX-ORCHID-<nonce>"),
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			codename := "VX-ORCHID-" + n
			stateID := strings.ToLower(n)
			captureHome := filepath.Join(runner.StateDir(), "hermes", "capture-"+stateID)
			recallHome := filepath.Join(runner.StateDir(), "hermes", "recall-"+stateID)
			capturePrompt := hermesCapturePrompt(codename)
			recallPrompt := "In Mira's launch-board update, what was the project codename and the handoff color? Return only the two values without calling tools or asking questions."
			expect := []string{strings.ToLower(codename), hermesStoryColor}

			user := runner.ConfiguredUser(b, "user")

			capture := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_capture", In: dag.In{"user_key": user},
				Config: dag.Cfg{"message": capturePrompt, "home": captureHome, "toolsets": "clarify"}})
			captureNoTools := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"home": capture.Out("home"), "after": capture},
				Config: dag.Cfg{"forbid_tools": hermesAutomaticForbiddenTools()},
			})

			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "wait_for_commit",
				In:   dag.In{"user_key": user, "session_id": capture.Out("session_id"), "after": captureNoTools},
				Config: dag.Cfg{
					"min_commits": 1, "min_extracted": 1,
					"settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true,
				}})

			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_story_memory",
				In:   dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query":        codename + " " + hermesStoryColor,
					"expect":       expect,
					"min_results":  1,
					"settle":       10,
					"retry":        8,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(codename),
				}})

			recall := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_recall",
				In:   dag.In{"user_key": user, "after": found},
				Config: dag.Cfg{
					"message":  recallPrompt,
					"home":     recallHome,
					"toolsets": "clarify",
				}})
			recallNoTools := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"home": recall.Out("home"), "after": recall},
				Config: dag.Cfg{"forbid_tools": hermesAutomaticForbiddenTools()},
			})

			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In:     dag.In{"text": recall.Out("reply"), "after": b.Merge(committed, found, recallNoTools)},
				Config: dag.Cfg{"expect": expect, "forbid": []string{"i do not know", "unknown", "not sure"}},
			})
		},
	}
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
