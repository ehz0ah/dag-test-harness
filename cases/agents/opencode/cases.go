package opencode

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	opencodeadapter "code.byted.org/data-arch/ovtest/adapters/opencode"
	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

var opencodeOpenVikingTools = []string{"health", "remember", "find", "search", "read", "add_resource", "list", "grep", "glob", "forget"}

func All() []runner.Case {
	return []runner.Case{
		automaticMemoryCase(),
		mcpToolsCase(),
	}
}

func automaticMemoryCase() runner.Case {
	const color = "saffron nickel"
	return runner.Case{
		ID:   "opencode-openviking-automatic-memory",
		Goal: "OpenCode captures a normal conversation turn through the OpenViking plugin and recalls it in a fresh OpenCode run.",
		Reference: `PASS only if:
- a real opencode run executes with the OpenViking plugin loaded and auto-capture enabled;
- the OpenCode plugin commits the captured session through OpenViking lifecycle hooks;
- OpenViking retrieval can find the extracted marker;
- a fresh opencode run answer recalls the marker through automatic OpenViking context, without the test writing the memory directly.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "OCAUTO-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "opencode", "automatic-memory-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			health := b.Add(ovops.Command, dag.Spec{
				Name: "openviking_health",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"args": []string{"health"},
				}})
			capture := b.Add(opencodeadapter.Exec, dag.Spec{
				Name: "chat_capture",
				In:   dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"message":                  opencodeAutomaticCapturePrompt(code, color),
					"project_dir":              filepath.Join(base, "capture"),
					"state_dir":                stateDir,
					"auto_capture":             true,
					"auto_recall":              false,
					"no_auto_inject":           true,
					"workspace_peer":           false,
					"commit_keep_recent_count": 0,
					"timeout":                  900,
				}})
			captureNoTools := b.Add(opencodeadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"jsonl": capture.Out("jsonl"), "after": capture},
				Config: dag.Cfg{"forbid_tools": opencodeOpenVikingTools},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_captured_session",
				In:   dag.In{"user_key": user, "session_id": capture.Out("ov_session_id"), "after": captureNoTools},
				Config: dag.Cfg{
					"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true,
				}})
			aliasFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_alias",
				In:   dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": code, "expect": []string{strings.ToLower(code)},
					"min_results": 1, "settle": 10, "retry": 24,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code),
				}})
			colorFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_color",
				In:   dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": color, "expect": []string{color},
					"min_results": 1, "settle": 10, "retry": 24,
				}})
			recall := b.Add(opencodeadapter.Exec, dag.Spec{
				Name: "chat_recall",
				In:   dag.In{"user_key": user, "after": b.Merge(aliasFound, colorFound)},
				Config: dag.Cfg{
					"message":        opencodeAutomaticRecallPrompt(),
					"project_dir":    filepath.Join(base, "recall"),
					"state_dir":      stateDir,
					"auto_capture":   false,
					"auto_recall":    true,
					"no_auto_inject": true,
					"workspace_peer": false,
					"timeout":        900,
				}})
			recallNoTools := b.Add(opencodeadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"jsonl": recall.Out("jsonl"), "after": recall},
				Config: dag.Cfg{"forbid_tools": opencodeOpenVikingTools},
			})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": recall.Out("reply"), "after": b.Merge(aliasFound, colorFound, recallNoTools)},
				Config: dag.Cfg{
					"expect": expect,
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}

func opencodeAutomaticCapturePrompt(code, color string) string {
	return fmt.Sprintf("This is an ordinary conversation. Do not call any tools or memory functions. These are stable personal preferences I want retained for future chats: my project alias is %s, and my preferred theme color is %s. Confirm both values in one short sentence.", code, color)
}

func opencodeAutomaticRecallPrompt() string {
	return "Using only context already supplied automatically, what are my project alias and preferred theme color from the earlier conversation? Do not call any tools or memory functions. Answer with only the alias and color."
}

func mcpToolsCase() runner.Case {
	const (
		markerColor = "saffron teal"
		markerFood  = "mango sticky rice"
	)
	return runner.Case{
		ID:        "opencode-openviking-mcp-tools",
		Goal:      "OpenCode uses the OpenViking MCP tool surface in one end-to-end case.",
		Reference: `PASS only if focused real opencode runs use the OpenViking MCP server to remember and find/search, ingest the committed local fixture through the signed upload flow, ingest and inspect the configured public remote resource, forget the exact remembered URI, and satisfy independent OpenViking postconditions.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "OCMCP-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "opencode", "mcp-tools-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expectMemory := support.MemoryFactExpect(code, markerColor, markerFood)
			resourceURL := opencodeRemoteResourceURL()
			resourceExpect := opencodeRemoteResourceExpect()
			resourceQuery := strings.Join(resourceExpect, " ")
			resourceRoot := support.ResourceRoot("opencode", stateID)
			resourceURI := resourceRoot + "/" + strings.ToLower(code) + ".md"
			localResourcePath := opencodeLocalResourceFixturePath()
			localResourceExpect := []string{"opal-river-409", "indigo silver"}
			localResourceQuery := strings.Join(localResourceExpect, " ")
			localResourceURI := resourceRoot + "/local-" + strings.ToLower(code) + ".md"
			user := runner.ConfiguredUser(b, "user")
			health := b.Add(ovops.Command, dag.Spec{
				Name: "openviking_health",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"args": []string{"health"},
				}})
			memoryTools := b.Add(opencodeadapter.Exec, dag.Spec{
				Name: "chat_mcp_memory",
				In:   dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"message":           opencodeMCPMemoryPrompt(code, markerColor, markerFood),
					"project_dir":       filepath.Join(base, "memory"),
					"state_dir":         stateDir,
					"auto_capture":      false,
					"auto_recall":       false,
					"no_auto_inject":    true,
					"workspace_peer":    false,
					"timeout":           600,
					"allow_empty_reply": true,
				}})
			memoryEvidence := b.Add(opencodeadapter.Evidence, dag.Spec{
				Name: "mcp_memory_evidence",
				In:   dag.In{"jsonl": memoryTools.Out("jsonl"), "reply": memoryTools.Out("reply"), "after": memoryTools},
				Config: dag.Cfg{
					"expect_tools": []string{"remember", "find", "search"},
				}})
			localTools := b.Add(opencodeadapter.Exec, dag.Spec{
				Name: "chat_mcp_local_resource",
				In:   dag.In{"user_key": user, "after": memoryEvidence},
				Config: dag.Cfg{
					"message":           opencodeMCPLocalResourcePrompt(localResourcePath, localResourceURI, localResourceQuery),
					"project_dir":       filepath.Join(base, "local-resource"),
					"state_dir":         stateDir,
					"auto_capture":      false,
					"auto_recall":       false,
					"no_auto_inject":    true,
					"workspace_peer":    false,
					"timeout":           600,
					"allow_empty_reply": true,
				}})
			localEvidence := b.Add(opencodeadapter.Evidence, dag.Spec{
				Name:   "mcp_local_resource_evidence",
				In:     dag.In{"jsonl": localTools.Out("jsonl"), "reply": localTools.Out("reply"), "after": localTools},
				Config: dag.Cfg{"expect_tools": []string{"add_resource"}},
			})
			remoteTools := b.Add(opencodeadapter.Exec, dag.Spec{
				Name: "chat_mcp_remote_resource",
				In:   dag.In{"user_key": user, "after": localEvidence},
				Config: dag.Cfg{
					"message":           opencodeMCPRemoteResourcePrompt(resourceURL, resourceURI, resourceRoot, resourceQuery),
					"project_dir":       filepath.Join(base, "remote-resource"),
					"state_dir":         stateDir,
					"auto_capture":      false,
					"auto_recall":       false,
					"no_auto_inject":    true,
					"workspace_peer":    false,
					"timeout":           600,
					"allow_empty_reply": true,
				}})
			remoteEvidence := b.Add(opencodeadapter.Evidence, dag.Spec{
				Name:   "mcp_remote_resource_evidence",
				In:     dag.In{"jsonl": remoteTools.Out("jsonl"), "reply": remoteTools.Out("reply"), "after": remoteTools},
				Config: dag.Cfg{"expect_tools": []string{"add_resource", "search", "list", "glob"}, "expect": resourceExpect},
			})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_memory_before_forget", In: dag.In{"user_key": user, "after": memoryEvidence},
				Config: dag.Cfg{"query": code + " " + markerColor + " " + markerFood, "expect": expectMemory, "uri": "viking://user/memories", "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			memoryURI := b.Add(sharedevidence.MemoryURI, dag.Spec{
				Name: "exact_memory_uri", In: dag.In{"memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"uri_prefix": "viking://user/"},
			})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait_for_resource_ingestion",
				In:   dag.In{"user_key": user, "after": b.Merge(localEvidence, remoteEvidence, memoryURI)},
				Config: dag.Cfg{
					"timeout": 240,
				}})
			remoteResourceFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_remote_resource",
				In:   dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{
					"query": resourceQuery, "expect": resourceExpect,
					"expect_uri":  strings.ToLower(resourceURI),
					"min_results": 1, "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot),
				}})
			localResourceFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_local_resource",
				In:   dag.In{"user_key": user, "after": wait},
				Config: dag.Cfg{
					"query": localResourceQuery, "expect": localResourceExpect,
					"expect_uri":  strings.ToLower(localResourceURI),
					"min_results": 1, "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot),
				}})
			forget := b.Add(opencodeadapter.Exec, dag.Spec{
				Name: "chat_mcp_forget_exact_uri",
				In:   dag.In{"user_key": user, "memory_uri": memoryURI.Out("uri"), "after": b.Merge(remoteResourceFound, localResourceFound)},
				Config: dag.Cfg{
					"message_template": "Use only the OpenViking MCP forget tool once on this exact URI: {{memory_uri}}. Do not search. Reply only FORGOTTEN.",
					"project_dir":      filepath.Join(base, "forget"), "state_dir": stateDir,
					"auto_capture": false, "auto_recall": false, "no_auto_inject": true,
					"workspace_peer": false, "timeout": 600, "allow_empty_reply": true,
				},
			})
			forgetEvidence := b.Add(opencodeadapter.Evidence, dag.Spec{
				Name: "mcp_forget_evidence", In: dag.In{"jsonl": forget.Out("jsonl"), "reply": forget.Out("reply"), "after": forget},
				Config: dag.Cfg{"expect_tools": []string{"forget"}},
			})
			forgotten := b.Add(ovops.URIAbsent, dag.Spec{
				Name:   "find_forgotten_memory",
				In:     dag.In{"user_key": user, "uri": memoryURI.Out("uri"), "after": forgetEvidence},
				Config: dag.Cfg{"settle": 5, "retry": 12},
			})
			b.Add(checks.Deterministic, dag.Spec{Name: "evidence_check",
				In: dag.In{"after": b.Merge(
					memoryEvidence.Out("ok"),
					localEvidence.Out("ok"),
					remoteEvidence.Out("ok"),
					memoryFound.Out("ok"),
					forgetEvidence.Out("ok"),
					remoteResourceFound.Out("ok"),
					localResourceFound.Out("ok"),
					forgotten.Out("ok"),
				)},
				Config: dag.Cfg{
					"explanation": "OpenCode MCP tool calls, resource retrieval, and forget verification all passed",
				}})
		},
	}
}

func opencodeLocalResourceFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("fixtures", "resources", "agent-memory.md")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "fixtures", "resources", "agent-memory.md"))
}

func opencodeRemoteResourceURL() string {
	return support.RemoteResourceURL("OV_TEST_OPENCODE_RESOURCE_URL")
}

func opencodeRemoteResourceExpect() []string {
	return support.RemoteResourceExpect("OV_TEST_OPENCODE_RESOURCE_EXPECT")
}

func opencodeMCPMemoryPrompt(code, markerColor, markerFood string) string {
	return fmt.Sprintf(`Use only OpenViking MCP tools. Execute these steps in order:
1. Call remember once with this fact: "Marker %s has color %s and associated food %s."
2. Call find once for "%s %s %s".
3. Call search once for "%s %s %s".
Then reply exactly: OPENCODE_MCP_MEMORY_PASS %s %s %s`,
		code, markerColor, markerFood,
		code, markerColor, markerFood,
		code, markerColor, markerFood,
		code, markerColor, markerFood)
}

func opencodeMCPLocalResourcePrompt(path, uri, query string) string {
	return fmt.Sprintf(`Use the OpenViking add_resource MCP tool and shell only for the signed upload.
1. Call add_resource exactly once with path=%q and to=%q. Copy both values verbatim; do not join, prefix, or rewrite them.
2. From that tool result, copy the signed temp_upload URL exactly. Run this shell command once: curl -fsS -X POST -F "file=@%s" "<signed temp_upload URL>".
Do not call add_resource again. After curl succeeds, reply exactly: OPENCODE_MCP_LOCAL_PASS %s`, path, uri, path, query)
}

func opencodeMCPRemoteResourcePrompt(path, uri, root, query string) string {
	return fmt.Sprintf(`Use only OpenViking MCP tools. Execute these steps in order:
1. Call add_resource exactly once with path=%q and to=%q. Copy both values verbatim; do not join, prefix, or rewrite them.
2. Call search once with query=%q and target_uri=%q.
3. Call list once with uri=%q and recursive=true.
4. Call glob once with uri=%q and pattern="**/*.md".
Then reply exactly: OPENCODE_MCP_REMOTE_PASS %s`, path, uri, query, root, root, root, query)
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
