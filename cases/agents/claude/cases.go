package claude

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	claudeadapter "code.byted.org/data-arch/ovtest/adapters/claude"
	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

var claudeOpenVikingTools = []string{"health", "remember", "find", "search", "read", "add_resource", "list", "grep", "glob", "forget"}

func claudeDisallowedOpenVikingTools() []string {
	out := make([]string, 0, len(claudeOpenVikingTools))
	for _, tool := range claudeOpenVikingTools {
		out = append(out, "mcp__plugin_openviking-memory_openviking__"+tool)
	}
	return out
}

func All() []runner.Case {
	return []runner.Case{
		automaticMemoryCase(),
		mcpToolsCase(),
		subagentLifecycleCase(),
	}
}

func automaticMemoryCase() runner.Case {
	const color = "violet bronze"
	return runner.Case{
		ID:   "claude-code-openviking-automatic-memory",
		Goal: "Claude Code captures a normal conversation turn through the OpenViking plugin and recalls it in a fresh Claude run.",
		Reference: `PASS only if:
- a real claude -p turn runs with auto-capture enabled;
- the Claude plugin commits the captured session through OpenViking lifecycle hooks;
- OpenViking retrieval can find the extracted marker;
- a fresh claude -p answer recalls the marker through automatic OpenViking context, without the test writing the memory directly.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CLAUTO-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "claude", "automatic-memory-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			health := b.Add(ovops.Command, dag.Spec{
				Name: "openviking_health",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"args": []string{"health"},
				}})
			capture := b.Add(claudeadapter.Exec, dag.Spec{
				Name: "chat_capture",
				In:   dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"message":                claudeAutomaticMemoryCapturePrompt(code, color),
					"cwd":                    filepath.Join(base, "capture"),
					"state_dir":              stateDir,
					"auto_capture":           true,
					"auto_recall":            false,
					"no_auto_inject":         true,
					"write_path_async":       false,
					"isolated_mcp":           true,
					"disable_mcp":            true,
					"bypass_permissions":     true,
					"disable_slash_commands": true,
					"disable_builtin_tools":  true,
					"disallowed_tools":       claudeDisallowedOpenVikingTools(),
					"setting_sources":        "",
					"session_id":             uuidLike(),
					"timeout":                600,
				}})
			captureNoTools := b.Add(claudeadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"jsonl": capture.Out("jsonl"), "after": capture},
				Config: dag.Cfg{"forbid_any_tool": true},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_captured_session",
				In:   dag.In{"user_key": user, "session_id": capture.Out("ov_session_id"), "after": captureNoTools},
				Config: dag.Cfg{
					"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true,
				}})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_memory",
				In:   dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 24,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code),
				}})
			recall := b.Add(claudeadapter.Exec, dag.Spec{
				Name: "chat_recall",
				In:   dag.In{"user_key": user, "after": found},
				Config: dag.Cfg{
					"message":                claudeAutomaticMemoryRecallPrompt(),
					"cwd":                    filepath.Join(base, "recall"),
					"state_dir":              stateDir,
					"auto_capture":           false,
					"auto_recall":            true,
					"recall_peer_scope":      "actor",
					"no_auto_inject":         true,
					"write_path_async":       false,
					"isolated_mcp":           true,
					"disable_mcp":            true,
					"bypass_permissions":     true,
					"disable_slash_commands": true,
					"disable_builtin_tools":  true,
					"disallowed_tools":       claudeDisallowedOpenVikingTools(),
					"setting_sources":        "",
					"timeout":                600,
				}})
			recallNoTools := b.Add(claudeadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"jsonl": recall.Out("jsonl"), "after": recall},
				Config: dag.Cfg{"forbid_any_tool": true},
			})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": recall.Out("reply"), "after": b.Merge(found, recallNoTools)},
				Config: dag.Cfg{
					"expect": expect,
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}

func claudeAutomaticMemoryCapturePrompt(label, color string) string {
	return fmt.Sprintf("For my small personal project, I prefer the project nickname %s and the interface theme color %s. Please repeat both choices in one short sentence.", label, color)
}

func claudeAutomaticMemoryRecallPrompt() string {
	return "Which project nickname and interface theme color do I prefer for my small personal project? Answer with only the nickname and color."
}

func subagentLifecycleCase() runner.Case {
	return runner.Case{
		ID:        "claude-code-openviking-subagent-lifecycle",
		Goal:      "Claude Code captures and commits a real child-agent transcript through SubagentStart and SubagentStop hooks.",
		Reference: "PASS only if Claude launches the declared child agent, stream JSON contains both lifecycle hooks, and OpenViking extracts the child's unique fact without MCP-tool bypass.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CLSUB-" + n
			color := "graphite teal"
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "claude", "subagent-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expect := []string{strings.ToLower(code), color}
			agentsRaw, _ := json.Marshal(map[string]any{
				"ovtest-child": map[string]any{
					"description": "Required ovtest child that reports one supplied fact",
					"prompt":      "You are the required ovtest child. Repeat the exact marker and color supplied by the parent, then stop.",
				},
			})
			user := runner.ConfiguredUser(b, "user")
			child := b.Add(claudeadapter.Exec, dag.Spec{
				Name: "run_child_agent", In: dag.In{"user_key": user},
				Config: dag.Cfg{
					"message": fmt.Sprintf("You must invoke the ovtest-child agent exactly once. Tell it that the child lifecycle marker is %s and the child color is %s. After it returns, reply only CHILD_DONE %s.", code, color, code),
					"cwd":     filepath.Join(base, "parent"), "state_dir": stateDir,
					"agents": string(agentsRaw), "allowed_tools": []string{"Agent"},
					"include_hook_events": true, "setting_sources": "", "write_path_async": false,
					"isolated_mcp": true, "disable_mcp": true,
					"auto_capture": true, "auto_recall": false, "no_auto_inject": true,
					"bypass_permissions": true, "disable_slash_commands": true, "timeout": 900,
				},
			})
			hooks := b.Add(claudeadapter.Evidence, dag.Spec{
				Name: "subagent_hook_evidence", In: dag.In{"jsonl": child.Out("jsonl"), "reply": child.Out("reply"), "after": child},
				Config: dag.Cfg{"expect_hooks": []string{"SubagentStart", "SubagentStop"}, "forbid_tools": claudeOpenVikingTools},
			})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait_for_child_commit", In: dag.In{"user_key": user, "after": hooks},
				Config: dag.Cfg{"timeout": 240},
			})
			parentPresent := b.Add(ovops.SessionPresent, dag.Spec{
				Name: "verify_parent_session", In: dag.In{"user_key": user, "session_id": child.Out("ov_session_id"), "after": hooks},
			})
			childCommitted := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_child_session",
				In:   dag.In{"user_key": user, "session_id": child.Out("child_ov_session_id"), "after": wait},
				Config: dag.Cfg{
					"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true,
				}})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_child_memory", In: dag.In{"user_key": user, "after": childCommitted},
				Config: dag.Cfg{"query": code + " " + color, "expect": expect, "settle": 10, "retry": 24,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			b.Add(checks.Deterministic, dag.Spec{
				Name: "evidence_check", In: dag.In{"after": b.Merge(hooks.Out("ok"), parentPresent.Out("ok"), childCommitted.Out("ok"), found.Out("ok"))},
				Config: dag.Cfg{"explanation": "Claude subagent lifecycle hooks and durable child memory passed"},
			})
		},
	}
}

func mcpToolsCase() runner.Case {
	const (
		markerColor = "saffron teal"
		markerFood  = "mango sticky rice"
	)
	return runner.Case{
		ID:        "claude-code-openviking-mcp-tools",
		Goal:      "Claude Code uses the OpenViking MCP tool surface in one end-to-end flow.",
		Reference: `PASS only if a real claude -p run uses the OpenViking MCP server to health-check, remember, find/search/read, ingest the configured public remote resource, ingest the committed local fixture through the signed upload flow, list/grep/glob/read resource content, forget the remembered URI, and verify the memory is gone.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CLMCP-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "claude", "mcp-tools-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expectMemory := support.MemoryFactExpect(code, markerColor, markerFood)
			resourceURL := claudeRemoteResourceURL()
			resourceExpect := claudeRemoteResourceExpect()
			resourceQuery := strings.Join(resourceExpect, " ")
			resourceRoot := support.ResourceRoot("claude-code", stateID)
			resourceURI := resourceRoot + "/" + strings.ToLower(code) + ".md"
			localResourcePath := claudeLocalResourceFixturePath()
			localResourceExpect := []string{"opal-river-409", "indigo silver"}
			localResourceQuery := strings.Join(localResourceExpect, " ")
			localResourceURI := resourceRoot + "/local-" + strings.ToLower(code) + ".md"
			finalExpect := append(append(append([]string{}, expectMemory...), resourceExpect...), localResourceExpect...)

			user := runner.ConfiguredUser(b, "user")
			health := b.Add(ovops.Command, dag.Spec{
				Name: "openviking_health",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"args": []string{"health"},
				}})
			tools := b.Add(claudeadapter.Exec, dag.Spec{
				Name: "chat_mcp_tools",
				In:   dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"message":                claudeMCPToolsPrompt(code, markerColor, markerFood, resourceRoot, resourceURI, resourceURL, resourceQuery, localResourceURI, localResourcePath, localResourceQuery),
					"cwd":                    filepath.Join(base, "tools"),
					"state_dir":              stateDir,
					"auto_capture":           false,
					"auto_recall":            false,
					"no_auto_inject":         true,
					"write_path_async":       false,
					"isolated_mcp":           true,
					"bypass_permissions":     true,
					"disable_slash_commands": true,
					"timeout":                900,
				}})
			evidence := b.Add(claudeadapter.Evidence, dag.Spec{
				Name: "mcp_tool_evidence",
				In:   dag.In{"jsonl": tools.Out("jsonl"), "reply": tools.Out("reply"), "after": tools},
				Config: dag.Cfg{
					"expect_tools": []string{
						"health",
						"remember",
						"find",
						"search",
						"read",
						"add_resource",
						"list",
						"grep",
						"glob",
					},
					"expect": finalExpect,
				}})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_memory_before_forget", In: dag.In{"user_key": user, "after": evidence},
				Config: dag.Cfg{"query": code + " " + markerColor + " " + markerFood, "expect": expectMemory, "uri": "viking://user/memories", "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			memoryURI := b.Add(sharedevidence.MemoryURI, dag.Spec{
				Name: "exact_memory_uri", In: dag.In{"memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"uri_prefix": "viking://user/"},
			})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait_for_resource_ingestion",
				In:   dag.In{"user_key": user, "after": b.Merge(evidence, memoryURI)},
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
			forget := b.Add(claudeadapter.Exec, dag.Spec{
				Name: "chat_mcp_forget_exact_uri",
				In:   dag.In{"user_key": user, "memory_uri": memoryURI.Out("uri"), "after": b.Merge(remoteResourceFound, localResourceFound)},
				Config: dag.Cfg{
					"message_template": "Use only the OpenViking MCP forget tool once on this exact URI: {{memory_uri}}. Do not search. Reply only FORGOTTEN.",
					"cwd":              filepath.Join(base, "forget"), "state_dir": stateDir,
					"auto_capture": false, "auto_recall": false, "no_auto_inject": true,
					"write_path_async": false, "bypass_permissions": true, "disable_slash_commands": true, "timeout": 600,
					"isolated_mcp": true,
				},
			})
			forgetEvidence := b.Add(claudeadapter.Evidence, dag.Spec{
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
					evidence.Out("ok"),
					memoryFound.Out("ok"),
					forgetEvidence.Out("ok"),
					remoteResourceFound.Out("ok"),
					localResourceFound.Out("ok"),
					forgotten.Out("ok"),
				)},
				Config: dag.Cfg{
					"explanation": "Claude MCP tool calls, resource retrieval, and forget verification all passed",
				}})
		},
	}
}

func claudeLocalResourceFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("fixtures", "resources", "agent-memory.md")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "fixtures", "resources", "agent-memory.md"))
}

func claudeRemoteResourceURL() string {
	return support.RemoteResourceURL("OV_TEST_CLAUDE_RESOURCE_URL")
}

func claudeRemoteResourceExpect() []string {
	return support.RemoteResourceExpect("OV_TEST_CLAUDE_RESOURCE_EXPECT")
}

func claudeMCPToolsPrompt(code, markerColor, markerFood, resourceRoot, resourceURI, resourceURL, resourceQuery, localResourceURI, localResourcePath, localResourceQuery string) string {
	return fmt.Sprintf(`Use OpenViking MCP tools for OpenViking operations. Do not use web browsing or file edits. Use shell only for the single curl upload required after local add_resource returns its signed upload URL.

Execute these steps in order:
1. Call OpenViking health.
2. Call OpenViking remember to store this single marker fact: "Marker %s has color %s and associated food %s."
3. Call OpenViking find for "%s %s %s".
4. Call OpenViking search for "%s %s %s".
5. Call OpenViking read on the concrete memory URI returned by find or search.
6. Call OpenViking add_resource on this remote URL: %s with to="%s".
7. Call OpenViking add_resource on this local file path: %s with to="%s". The tool should return a signed temp_upload URL. Use shell curl to POST the file bytes to that exact URL as multipart/form-data field "file": curl -fsS -X POST -F "file=@%s" "<signed upload URL>". Do not call add_resource a second time for this local file.
8. After both resources are indexed, call OpenViking search for "%s" scoped to "%s".
9. Call OpenViking read on the concrete remote resource URI returned by search. If search returns no concrete URI, read "%s".
10. Call OpenViking search for "%s" scoped to "%s".
11. Call OpenViking read on the concrete local resource URI returned by search. If search returns no concrete URI, read "%s".
12. Call OpenViking list on "%s" with recursive=true.
13. Call OpenViking grep for "opal-river-409" under "%s".
14. Call OpenViking glob with pattern "**/*.md" under "%s".
After every tool result succeeds, reply exactly:
CLAUDE_MCP_TOOLS_PASS %s %s %s %s %s`,
		code, markerColor, markerFood,
		code, markerColor, markerFood,
		code, markerColor, markerFood,
		resourceURL,
		resourceURI,
		localResourcePath,
		localResourceURI,
		localResourcePath,
		resourceQuery,
		resourceRoot,
		resourceURI,
		localResourceQuery,
		resourceRoot,
		localResourceURI,
		resourceRoot,
		resourceRoot,
		resourceRoot,
		code, markerColor, markerFood, resourceQuery, localResourceQuery)
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func uuidLike() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
