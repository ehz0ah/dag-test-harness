package pi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	piadapter "code.byted.org/data-arch/ovtest/adapters/pi"
	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

func All() []runner.Case {
	return []runner.Case{automaticMemoryCase(), toolsCase(), takeoverCase()}
}

func automaticMemoryCase() runner.Case {
	const color = "copper lilac"
	return runner.Case{
		ID:        "pi-openviking-automatic-memory",
		Goal:      "Pi captures an ordinary turn through its OpenViking extension and recalls it automatically in a fresh Pi session.",
		Reference: "PASS only if Pi extension lifecycle events capture and commit the first session, OpenViking extracts the unique fact, and a fresh tool-free Pi session receives and answers from automatic recall context.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "PIAUTO-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "pi", "automatic-memory-"+stateID)
			stateDir := filepath.Join(base, "extension-state")
			expect := []string{strings.ToLower(code), color}
			user := runner.ConfiguredUser(b, "user")

			capture := b.Add(piadapter.Exec, dag.Spec{
				Name: "chat_capture", In: dag.In{"user_key": user},
				Config: dag.Cfg{
					"message":     piAutomaticMemoryCapturePrompt(code, color),
					"project_dir": filepath.Join(base, "capture"), "state_dir": stateDir,
					"auto_capture": true, "takeover": false, "disable_tools": true, "timeout": 900,
				},
			})
			captureEvidence := b.Add(piadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"jsonl": capture.Out("jsonl"), "after": capture},
				Config: dag.Cfg{"forbid_any_tool": true},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_captured_session", In: dag.In{"user_key": user, "session_id": capture.Out("ov_session_id"), "after": captureEvidence},
				Config: dag.Cfg{"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true},
			})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_memory", In: dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect, "settle": 10, "retry": 24,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code),
				},
			})
			recall := b.Add(piadapter.Exec, dag.Spec{
				Name: "chat_recall", In: dag.In{"user_key": user, "after": found},
				Config: dag.Cfg{
					"message":     piAutomaticMemoryRecallPrompt(),
					"project_dir": filepath.Join(base, "recall"), "state_dir": stateDir,
					"auto_capture": false, "takeover": false, "disable_tools": true, "timeout": 900,
					"recall_peer_scope": "actor", "score_threshold": "0.35",
				},
			})
			recallEvidence := b.Add(piadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"jsonl": recall.Out("jsonl"), "after": recall},
				Config: dag.Cfg{"forbid_any_tool": true},
			})
			b.Add(checks.Text, dag.Spec{
				Name: "reply_check", In: dag.In{"text": recall.Out("reply"), "after": b.Merge(found.Out("ok"), recallEvidence.Out("ok"))},
				Config: dag.Cfg{"expect": expect, "forbid": []string{"unknown", "not sure", "do not know"}},
			})
		},
	}
}

func piAutomaticMemoryCapturePrompt(code, color string) string {
	return fmt.Sprintf("For my Pi coding workspace, I prefer the scratch-project name %s and the interface theme color %s. Do not call tools. Please repeat both choices briefly.", code, color)
}

func piAutomaticMemoryRecallPrompt() string {
	return "Using only automatically supplied context, what scratch-project name and interface theme color do I prefer for my Pi coding workspace? Do not call tools. Answer only with the name and color."
}

func toolsCase() runner.Case {
	const color = "amber graphite"
	return runner.Case{
		ID:        "pi-openviking-tools",
		Goal:      "Pi exercises the native OpenViking extension tool and resource workflow with independent server-side postconditions.",
		Reference: "PASS only if completed, non-error Pi tool events prove remember, search, read, browse, archive expansion, remote resource ingestion, and exact forget; OpenViking independently verifies extraction, resource retrieval, and deletion.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "PITOOL-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "pi", "tools-"+stateID)
			stateDir := filepath.Join(base, "extension-state")
			memoryExpect := support.MemoryFactExpect(code, color)
			resourceURL := support.RemoteResourceURL("OV_TEST_PI_RESOURCE_URL")
			resourceExpect := support.RemoteResourceExpect("OV_TEST_PI_RESOURCE_EXPECT")
			resourceQuery := strings.Join(resourceExpect, " ")
			user := runner.ConfiguredUser(b, "user")

			seed := b.Add(piadapter.Exec, dag.Spec{
				Name: "remember_memory", In: dag.In{"user_key": user},
				Config: dag.Cfg{
					"message":     fmt.Sprintf("Call viking_remember exactly once with: Marker %s has color %s. Then reply only REMEMBERED %s.", code, color, code),
					"project_dir": filepath.Join(base, "remember"), "state_dir": stateDir,
					"auto_capture": false, "takeover": false, "timeout": 900,
				},
			})
			rememberEvidence := b.Add(piadapter.Evidence, dag.Spec{
				Name: "remember_evidence", In: dag.In{"jsonl": seed.Out("jsonl"), "after": seed},
				Config: dag.Cfg{"expect_tools": []string{"viking_remember"}},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_remembered_session", In: dag.In{"user_key": user, "session_id": seed.Out("ov_session_id"), "after": rememberEvidence},
				Config: dag.Cfg{"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true},
			})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_memory_before_forget", In: dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": memoryExpect, "uri": "viking://user/memories", "settle": 10, "retry": 24,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code),
				},
			})
			memoryURI := b.Add(sharedevidence.MemoryURI, dag.Spec{
				Name: "exact_memory_uri", In: dag.In{"memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"uri_prefix": "viking://user/"},
			})
			workflow := b.Add(piadapter.Exec, dag.Spec{
				Name: "native_tool_workflow",
				In: dag.In{
					"user_key": user, "memory_uri": memoryURI.Out("uri"), "archive_session_id": seed.Out("ov_session_id"), "after": memoryURI,
				},
				Config: dag.Cfg{
					"message_template": fmt.Sprintf(`Use the native OpenViking Pi tools, in this order:
1. viking_search for %s %s.
2. viking_read this exact URI at full level: {{memory_uri}}
3. viking_browse list viking://user/memories.
4. viking_archive_expand with session_id={{archive_session_id}}.
5. viking_add_resource for this exact URL: %s
6. viking_forget this exact URI: {{memory_uri}} (do not search for deletion).
Reply only PI_TOOLS_PASS %s.`, code, color, resourceURL, code),
					"project_dir": filepath.Join(base, "workflow"), "state_dir": stateDir,
					"auto_capture": false, "takeover": false, "timeout": 1200,
				},
			})
			toolEvidence := b.Add(piadapter.Evidence, dag.Spec{
				Name: "native_tool_evidence", In: dag.In{"jsonl": workflow.Out("jsonl"), "after": workflow},
				Config: dag.Cfg{"expect_tools": []string{
					"viking_search", "viking_read", "viking_browse", "viking_archive_expand", "viking_add_resource", "viking_forget",
				}},
			})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait_for_resource", In: dag.In{"user_key": user, "after": toolEvidence},
				Config: dag.Cfg{"timeout": 240},
			})
			resourceFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_remote_resource", In: dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{"query": resourceQuery, "expect": resourceExpect, "settle": 10, "retry": 24},
			})
			forgotten := b.Add(ovops.URIAbsent, dag.Spec{
				Name: "verify_memory_forgotten", In: dag.In{"user_key": user, "uri": memoryURI.Out("uri"), "after": toolEvidence},
				Config: dag.Cfg{"settle": 5, "retry": 12},
			})
			b.Add(checks.Deterministic, dag.Spec{
				Name: "evidence_check", In: dag.In{"after": b.Merge(rememberEvidence.Out("ok"), toolEvidence.Out("ok"), resourceFound.Out("ok"), forgotten.Out("ok"))},
				Config: dag.Cfg{"explanation": "Pi native tools and independent OpenViking postconditions passed"},
			})
		},
	}
}

func takeoverCase() runner.Case {
	const color = "obsidian mint"
	return runner.Case{
		ID:        "pi-openviking-takeover-compaction",
		Goal:      "Pi exercises OpenViking context takeover through capture, archive commit, and the native compaction lifecycle.",
		Reference: "PASS only if Pi emits a successful manual compaction lifecycle after the OpenViking takeover threshold, the extension-created session is committed and extracted, and a fresh tool-free Pi session recalls the archived fact.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "PITAKE-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "pi", "takeover-"+stateID)
			stateDir := filepath.Join(base, "extension-state")
			expect := []string{strings.ToLower(code), color}
			user := runner.ConfiguredUser(b, "user")

			compact := b.Add(piadapter.Exec, dag.Spec{
				Name: "capture_and_compact", In: dag.In{"user_key": user},
				Config: dag.Cfg{
					"message":     fmt.Sprintf("Do not call tools. The takeover drill marker is %s and its color is %s. Repeat both once.", code, color),
					"project_dir": filepath.Join(base, "compact"), "state_dir": stateDir,
					"auto_capture": true, "takeover": true, "takeover_token_threshold": 1,
					"takeover_keep_recent_turns": 0, "compaction_keep_recent_tokens": 1,
					"compact_after": true, "disable_tools": true, "timeout": 1200,
				},
			})
			compactEvidence := b.Add(piadapter.Evidence, dag.Spec{
				Name: "takeover_compaction_evidence", In: dag.In{"jsonl": compact.Out("jsonl"), "after": compact},
				Config: dag.Cfg{"forbid_any_tool": true, "expect_compaction": true},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_takeover_session", In: dag.In{"user_key": user, "session_id": compact.Out("ov_session_id"), "after": compactEvidence},
				Config: dag.Cfg{"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true},
			})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_takeover_memory", In: dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect, "settle": 10, "retry": 24,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code),
				},
			})
			recall := b.Add(piadapter.Exec, dag.Spec{
				Name: "recall_after_takeover", In: dag.In{"user_key": user, "after": found},
				Config: dag.Cfg{
					"message":     "Using only automatic context, state the takeover drill marker and color. Do not call tools.",
					"project_dir": filepath.Join(base, "recall"), "state_dir": stateDir,
					"auto_capture": false, "takeover": false, "disable_tools": true, "timeout": 900,
					"recall_peer_scope": "actor", "score_threshold": "0.35",
				},
			})
			recallEvidence := b.Add(piadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"jsonl": recall.Out("jsonl"), "after": recall},
				Config: dag.Cfg{"forbid_any_tool": true},
			})
			b.Add(checks.Text, dag.Spec{
				Name: "reply_check", In: dag.In{"text": recall.Out("reply"), "after": b.Merge(compactEvidence.Out("ok"), found.Out("ok"), recallEvidence.Out("ok"))},
				Config: dag.Cfg{"expect": expect, "forbid": []string{"unknown", "not sure", "do not know"}},
			})
		},
	}
}

func nonce(bytes int) string {
	buffer := make([]byte, bytes)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)
}
