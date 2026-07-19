package openclaw

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	openclawadapter "code.byted.org/data-arch/ovtest/adapters/openclaw"
	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

func localResourceFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("fixtures", "resources", "agent-memory.md")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "fixtures", "resources", "agent-memory.md"))
}

func remoteResourceURL() string {
	return support.RemoteResourceURL("")
}

func remoteResourceExpect() []string {
	return support.RemoteResourceExpect("")
}

func toolMemoryExpect(code string) []string {
	return support.MemoryFactExpect(code, "amber violet")
}

func toolsCase() runner.Case {
	const color = "amber violet"
	return runner.Case{
		ID:        "openclaw-openviking-tools",
		Goal:      "OpenClaw completes the native OpenViking memory and resource workflow with exact external postconditions.",
		Reference: "PASS only if structured OpenClaw transcript evidence proves successful native tool results across isolated turns, the memory and resources exist, and memory_forget deletes the exact verified URI.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CLAWTOOL-" + n
			stateID := strings.ToLower(n)
			stateDir := filepath.Join(runner.StateDir(), "openclaw", "tools-"+stateID)
			localPath := localResourceFixturePath()
			remoteURL := remoteResourceURL()
			memoryExpect := toolMemoryExpect(code)
			localExpect := []string{"opal-river-409", "indigo silver"}
			remoteExpect := remoteResourceExpect()
			resourceRoot := support.ResourceRoot("openclaw", stateID)
			localURI := resourceRoot + "/local-" + strings.ToLower(code) + ".md"
			remoteURI := resourceRoot + "/remote-" + strings.ToLower(code) + ".md"
			user := runner.ConfiguredUser(b, "user")

			store := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_memory_store", In: dag.In{"user_key": user},
				Config: dag.Cfg{
					"session_id": "ovclaw-store-" + stateID, "state_dir": stateDir, "timeout": 600, "agent_timeout": 300, "auto_capture": false, "auto_recall": false,
					"message": fmt.Sprintf("Call memory_store exactly once to store note 'OpenClaw marker %s has color %s'. Then reply only STORED %s.", code, color, code),
				},
			})
			storeEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "memory_store_evidence", In: dag.In{"transcript": store.Out("transcript"), "after": store},
				Config: dag.Cfg{"expect_tools": []string{"memory_store"}},
			})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_memory_before_forget", In: dag.In{"user_key": user, "after": storeEvidence},
				Config: dag.Cfg{"query": code + " " + color, "expect": memoryExpect, "uri": "viking://user/memories", "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			memoryURI := b.Add(sharedevidence.MemoryURI, dag.Spec{
				Name: "exact_memory_uri", In: dag.In{"memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"uri_prefix": "viking://user/"},
			})
			recall := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_memory_recall", In: dag.In{"user_key": user, "after": memoryURI},
				Config: dag.Cfg{
					"session_id": "ovclaw-recall-tool-" + stateID, "state_dir": stateDir, "timeout": 600, "agent_timeout": 300, "auto_capture": false, "auto_recall": false,
					"message": fmt.Sprintf("Call memory_recall exactly once for '%s %s'. Then reply only RECALLED %s.", code, color, code),
				},
			})
			recallEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "memory_recall_evidence", In: dag.In{"transcript": recall.Out("transcript"), "after": recall},
				Config: dag.Cfg{"expect_tools": []string{"memory_recall"}},
			})
			search := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_memory_search", In: dag.In{"user_key": user, "after": recallEvidence},
				Config: dag.Cfg{
					"session_id": "ovclaw-search-" + stateID, "state_dir": stateDir, "timeout": 600, "agent_timeout": 300, "auto_capture": false, "auto_recall": false,
					"message": fmt.Sprintf("Call ov_search exactly once for '%s %s'. Then reply only SEARCHED %s.", code, color, code),
				},
			})
			searchEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "memory_search_evidence", In: dag.In{"transcript": search.Out("transcript"), "after": search},
				Config: dag.Cfg{"expect_tools": []string{"ov_search"}},
			})
			read := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_memory_read", In: dag.In{"user_key": user, "memory_uri": memoryURI.Out("uri"), "after": searchEvidence},
				Config: dag.Cfg{
					"session_id": "ovclaw-read-" + stateID, "state_dir": stateDir, "timeout": 600, "agent_timeout": 300, "auto_capture": false, "auto_recall": false,
					"message_template": "Call ov_read exactly once on this exact URI: {{memory_uri}}. Then reply only READ_DONE.",
				},
			})
			readEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "memory_read_evidence", In: dag.In{"transcript": read.Out("transcript"), "after": read},
				Config: dag.Cfg{"expect_tools": []string{"ov_read"}},
			})
			localWorkflow := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_local_resource", In: dag.In{"user_key": user, "after": readEvidence},
				Config: dag.Cfg{
					"session_id": "ovclaw-local-" + stateID, "state_dir": stateDir, "timeout": 900, "agent_timeout": 600, "auto_capture": false, "auto_recall": false,
					"message": fmt.Sprintf("Use add_resource exactly once to add local resource %s to %s and wait for indexing. Reply only LOCAL_DONE %s.", localPath, localURI, code),
				},
			})
			localEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "local_resource_evidence", In: dag.In{"transcript": localWorkflow.Out("transcript"), "after": localWorkflow},
				Config: dag.Cfg{"expect_tools": []string{"add_resource"}},
			})
			remoteWorkflow := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_remote_resource", In: dag.In{"user_key": user, "after": localEvidence},
				Config: dag.Cfg{
					"session_id": "ovclaw-remote-" + stateID, "state_dir": stateDir, "timeout": 900, "agent_timeout": 600, "auto_capture": false, "auto_recall": false,
					"message": fmt.Sprintf("Use add_resource exactly once to add remote resource %s to %s and wait for indexing. Reply only REMOTE_DONE %s.", remoteURL, remoteURI, code),
				},
			})
			remoteEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "remote_resource_evidence", In: dag.In{"transcript": remoteWorkflow.Out("transcript"), "after": remoteWorkflow},
				Config: dag.Cfg{"expect_tools": []string{"add_resource"}},
			})
			localFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_local_resource", In: dag.In{"user_key": user, "after": localEvidence},
				Config: dag.Cfg{"query": strings.Join(localExpect, " "), "expect": localExpect, "expect_uri": strings.ToLower(localURI), "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot)},
			})
			remoteFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_remote_resource", In: dag.In{"user_key": user, "after": remoteEvidence},
				Config: dag.Cfg{"query": strings.Join(remoteExpect, " "), "expect": remoteExpect, "expect_uri": strings.ToLower(remoteURI), "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot)},
			})
			forget := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_forget_exact_uri",
				In:   dag.In{"user_key": user, "memory_uri": memoryURI.Out("uri"), "after": b.Merge(memoryURI, localFound, remoteFound)},
				Config: dag.Cfg{
					"session_id": "ovclaw-forget-" + stateID, "state_dir": stateDir,
					"auto_capture": false, "auto_recall": false,
					"message_template": "Use memory_forget once on this exact URI: {{memory_uri}}. Do not search. Reply only FORGOTTEN.",
				},
			})
			forgetEvidence := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "forget_evidence", In: dag.In{"transcript": forget.Out("transcript"), "after": forget},
				Config: dag.Cfg{"expect_tools": []string{"memory_forget"}},
			})
			gone := b.Add(ovops.URIAbsent, dag.Spec{
				Name: "find_memory_after_forget", In: dag.In{"user_key": user, "uri": memoryURI.Out("uri"), "after": forgetEvidence},
				Config: dag.Cfg{"settle": 5, "retry": 12},
			})
			b.Add(checks.Deterministic, dag.Spec{
				Name: "evidence_check", In: dag.In{"after": b.Merge(storeEvidence.Out("ok"), recallEvidence.Out("ok"), searchEvidence.Out("ok"), readEvidence.Out("ok"), localEvidence.Out("ok"), remoteEvidence.Out("ok"), memoryFound.Out("ok"), localFound.Out("ok"), remoteFound.Out("ok"), forgetEvidence.Out("ok"), gone.Out("ok"))},
				Config: dag.Cfg{"explanation": "OpenClaw native tool workflow and exact postconditions passed"},
			})
		},
	}
}

func compactionCase() runner.Case {
	return runner.Case{
		ID:        "openclaw-openviking-compaction",
		Goal:      "OpenClaw exercises OpenViking-owned context-engine compaction and reconstructs context afterward.",
		Reference: "PASS only if the real sessions.compact RPC returns ok=true and compacted=true, OpenViking commits/extracts the session, and a subsequent tool-free turn recalls the compacted fact.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CLAWCOMPACT-" + n
			stateID := strings.ToLower(n)
			stateDir := filepath.Join(runner.StateDir(), "openclaw", "compaction-"+stateID)
			expect := []string{strings.ToLower(code), "cobalt silver"}
			user := runner.ConfiguredUser(b, "user")
			capture := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_capture", In: dag.In{"user_key": user},
				Config: dag.Cfg{"session_id": "ovclaw-compact-" + stateID, "state_dir": stateDir, "auto_capture": true, "auto_recall": false, "commit_token_threshold_ratio": 1, "message": fmt.Sprintf("The context-engine drill marker is %s and its color is cobalt silver. Acknowledge without tools.", code)},
			})
			captureNoTools := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"transcript": capture.Out("transcript"), "after": capture},
				Config: dag.Cfg{"forbid_tools": openclawMemoryTools},
			})
			compact := b.Add(openclawadapter.Compact, dag.Spec{
				Name: "compact_session", In: dag.In{"session_key": capture.Out("session_key"), "after": captureNoTools},
				Config: dag.Cfg{"state_dir": stateDir, "auto_capture": true, "auto_recall": false, "commit_token_threshold_ratio": 1, "compaction_timeout_seconds": 540, "timeout": 600},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_compacted_session", In: dag.In{"user_key": user, "session_id": capture.Out("ov_session_id"), "after": compact},
				Config: dag.Cfg{"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true},
			})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_compacted_memory", In: dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{"query": code + " cobalt silver", "expect": expect, "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			recall := b.Add(openclawadapter.Chat, dag.Spec{
				Name: "chat_after_compaction", In: dag.In{"user_key": user, "after": found},
				Config: dag.Cfg{"session_id": "ovclaw-compact-" + stateID, "state_dir": stateDir, "auto_capture": false, "auto_recall": true, "message": "What are the context-engine drill marker and color? Answer directly without tools."},
			})
			recallNoTools := b.Add(openclawadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"transcript": recall.Out("transcript"), "after": recall},
				Config: dag.Cfg{"forbid_tools": openclawMemoryTools},
			})
			b.Add(checks.Text, dag.Spec{
				Name: "reply_check", In: dag.In{"text": recall.Out("reply"), "after": b.Merge(compact.Out("ok"), found.Out("ok"), recallNoTools)},
				Config: dag.Cfg{"expect": expect, "forbid": []string{"i do not know", "unknown", "not sure"}},
			})
		},
	}
}
