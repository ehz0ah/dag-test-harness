package codex

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	codexadapter "code.byted.org/data-arch/ovtest/adapters/codex"
	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

var codexOpenVikingTools = []string{"health", "remember", "find", "search", "read", "add_resource", "list", "grep", "glob", "forget"}

func All() []runner.Case {
	return []runner.Case{
		automaticMemoryCase(),
		mcpToolsCase(),
	}
}

func automaticMemoryCase() runner.Case {
	const color = "cerulean copper"
	return runner.Case{
		ID:   "codex-openviking-automatic-memory",
		Goal: "Codex captures a normal conversation turn through the OpenViking plugin and recalls it in a fresh Codex run.",
		Reference: `PASS only if:
- a real codex exec turn runs with auto-capture enabled;
- a fresh codex exec startup commits the previous captured session through the plugin;
- OpenViking retrieval can find the extracted marker;
- a fresh codex exec answer recalls the marker through automatic OpenViking context, without the test writing the memory directly.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CXAUTO-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "codex", "automatic-memory-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			health := b.Add(ovops.Command, dag.Spec{
				Name: "openviking_health",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"args": []string{"health"},
				}})
			capture := b.Add(codexadapter.Exec, dag.Spec{
				Name: "chat_capture",
				In:   dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"message":          fmt.Sprintf("This is a normal project update, not a tool request. In today's release notebook, the Codex automatic memory marker is %s and the checksum color is %s. Reply exactly: CAPTURED %s", code, color, code),
					"cwd":              filepath.Join(base, "capture"),
					"state_dir":        stateDir,
					"auto_capture":     true,
					"auto_recall":      false,
					"bypass_approvals": true,
					"timeout":          600,
				}})
			captureNoTools := b.Add(codexadapter.Evidence, dag.Spec{
				Name: "capture_no_tool_bypass", In: dag.In{"jsonl": capture.Out("jsonl"), "after": capture},
				Config: dag.Cfg{"forbid_tools": codexOpenVikingTools},
			})
			trigger := b.Add(codexadapter.Exec, dag.Spec{
				Name: "chat_commit_trigger",
				In:   dag.In{"user_key": user, "after": captureNoTools},
				Config: dag.Cfg{
					"message":          "Reply exactly COMMIT_TRIGGER_READY.",
					"cwd":              filepath.Join(base, "commit-trigger"),
					"state_dir":        stateDir,
					"auto_capture":     false,
					"auto_recall":      false,
					"bypass_approvals": true,
					"timeout":          600,
				}})
			triggerNoTools := b.Add(codexadapter.Evidence, dag.Spec{
				Name: "trigger_no_tool_bypass", In: dag.In{"jsonl": trigger.Out("jsonl"), "after": trigger},
				Config: dag.Cfg{"forbid_tools": codexOpenVikingTools},
			})
			committed := b.Add(ovops.SessionCommitted, dag.Spec{
				Name: "verify_captured_session",
				In:   dag.In{"user_key": user, "session_id": capture.Out("ov_session_id"), "after": triggerNoTools},
				Config: dag.Cfg{
					"min_commits": 1, "min_extracted": 1, "settle": 5, "retry": 120,
					"poll_commit_task": true, "cleanup_added_memories": true,
				}})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_extracted_memory",
				In:   dag.In{"user_key": user, "after": committed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code),
				}})
			recall := b.Add(codexadapter.Exec, dag.Spec{
				Name: "chat_recall",
				In:   dag.In{"user_key": user, "after": found},
				Config: dag.Cfg{
					"message":          "What are the Codex automatic memory marker and checksum color from the prior release notebook? Answer with only the marker and color.",
					"cwd":              filepath.Join(base, "recall"),
					"state_dir":        stateDir,
					"auto_capture":     false,
					"auto_recall":      true,
					"bypass_approvals": true,
					"timeout":          600,
				}})
			recallNoTools := b.Add(codexadapter.Evidence, dag.Spec{
				Name: "recall_no_tool_bypass", In: dag.In{"jsonl": recall.Out("jsonl"), "after": recall},
				Config: dag.Cfg{"forbid_tools": codexOpenVikingTools},
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

func mcpToolsCase() runner.Case {
	const (
		markerColor = "saffron teal"
		markerFood  = "mango sticky rice"
	)
	return runner.Case{
		ID:        "codex-openviking-mcp-tools",
		Goal:      "Codex uses the OpenViking MCP tool surface in one end-to-end flow.",
		Reference: `PASS only if a real codex exec run uses the OpenViking MCP server to health-check, remember, find/search/read, ingest the configured public remote resource, ingest the committed local fixture through the signed upload flow, list/grep/glob/read resource content, forget the remembered URI, and verify the memory is gone.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "CXMCP-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "codex", "mcp-tools-"+stateID)
			stateDir := filepath.Join(base, "plugin-state")
			expectMemory := support.MemoryFactExpect(code, markerColor, markerFood)
			resourceURL := codexRemoteResourceURL()
			resourceExpect := codexRemoteResourceExpect()
			resourceQuery := strings.Join(resourceExpect, " ")
			resourceRoot := support.ResourceRoot("codex", stateID)
			resourceURI := resourceRoot + "/" + strings.ToLower(code) + ".md"
			localResourcePath := codexLocalResourceFixturePath()
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
			submit := b.Add(codexadapter.Exec, dag.Spec{
				Name: "chat_mcp_submit",
				In:   dag.In{"user_key": user, "after": health},
				Config: dag.Cfg{
					"message":          codexMCPSubmitPrompt(code, markerColor, markerFood, resourceURI, resourceURL, resourceQuery, localResourceURI, localResourcePath, localResourceQuery),
					"cwd":              filepath.Join(base, "tools"),
					"state_dir":        stateDir,
					"auto_capture":     false,
					"auto_recall":      false,
					"isolated_mcp":     true,
					"bypass_approvals": true,
					"timeout":          900,
				}})
			submitEvidence := b.Add(codexadapter.Evidence, dag.Spec{
				Name: "mcp_submit_evidence",
				In:   dag.In{"jsonl": submit.Out("jsonl"), "reply": submit.Out("reply"), "after": submit},
				Config: dag.Cfg{
					"expect_tools": []string{
						"health",
						"remember",
						"find",
						"search",
						"read",
						"add_resource",
					},
					"expect": append(append(append([]string{}, expectMemory...), resourceExpect...), localResourceExpect...),
				}})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_memory_before_forget", In: dag.In{"user_key": user, "after": submitEvidence},
				Config: dag.Cfg{"query": code + " " + markerColor + " " + markerFood, "expect": expectMemory, "uri": "viking://user/memories", "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			memoryURI := b.Add(sharedevidence.MemoryURI, dag.Spec{
				Name: "exact_memory_uri", In: dag.In{"memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"uri_prefix": "viking://user/"},
			})
			wait := b.Add(ovops.Wait, dag.Spec{
				Name: "wait_for_resource_ingestion",
				In:   dag.In{"user_key": user, "after": b.Merge(submitEvidence, memoryURI)},
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
			resourceTools := b.Add(codexadapter.Exec, dag.Spec{
				Name: "chat_mcp_resource_tools",
				In:   dag.In{"user_key": user, "after": b.Merge(remoteResourceFound, localResourceFound)},
				Config: dag.Cfg{
					"message":          codexMCPResourceToolsPrompt(code, markerColor, markerFood, resourceRoot, resourceURI, resourceQuery, localResourceURI, localResourceQuery, localResourceExpect[0]),
					"cwd":              filepath.Join(base, "resource-tools"),
					"state_dir":        stateDir,
					"auto_capture":     false,
					"auto_recall":      false,
					"isolated_mcp":     true,
					"bypass_approvals": true,
					"timeout":          900,
				}})
			resourceEvidence := b.Add(codexadapter.Evidence, dag.Spec{
				Name: "mcp_resource_tool_evidence",
				In:   dag.In{"jsonl": resourceTools.Out("jsonl"), "reply": resourceTools.Out("reply"), "after": resourceTools},
				Config: dag.Cfg{
					"expect_tools": []string{
						"search",
						"read",
						"list",
						"grep",
						"glob",
					},
					"expect": append(append([]string{}, resourceExpect...), localResourceExpect...),
				}})
			forget := b.Add(codexadapter.Exec, dag.Spec{
				Name: "chat_mcp_forget_exact_uri",
				In:   dag.In{"user_key": user, "memory_uri": memoryURI.Out("uri"), "after": resourceEvidence},
				Config: dag.Cfg{
					"message_template": "Use only the OpenViking MCP forget tool once on this exact URI: {{memory_uri}}. Do not search. Reply only FORGOTTEN.",
					"cwd":              filepath.Join(base, "forget"), "state_dir": stateDir,
					"auto_capture": false, "auto_recall": false, "isolated_mcp": true,
					"bypass_approvals": true, "timeout": 600,
				},
			})
			forgetEvidence := b.Add(codexadapter.Evidence, dag.Spec{
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
					submitEvidence.Out("ok"),
					resourceEvidence.Out("ok"),
					memoryFound.Out("ok"),
					forgetEvidence.Out("ok"),
					remoteResourceFound.Out("ok"),
					localResourceFound.Out("ok"),
					forgotten.Out("ok"),
				)},
				Config: dag.Cfg{
					"explanation": "Codex MCP tool calls, resource retrieval, and forget verification all passed",
				}})
		},
	}
}

func codexLocalResourceFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("fixtures", "resources", "agent-memory.md")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "fixtures", "resources", "agent-memory.md"))
}

func codexRemoteResourceURL() string {
	return support.RemoteResourceURL("OV_TEST_CODEX_RESOURCE_URL")
}

func codexRemoteResourceExpect() []string {
	return support.RemoteResourceExpect("OV_TEST_CODEX_RESOURCE_EXPECT")
}

func codexMCPSubmitPrompt(code, markerColor, markerFood, resourceURI, resourceURL, resourceQuery, localResourceURI, localResourcePath, localResourceQuery string) string {
	return fmt.Sprintf(`Use OpenViking MCP tools for OpenViking operations. Do not use web browsing or file edits. Use shell only for the single curl upload required after local add_resource returns its signed upload URL.

Execute these steps in order:
1. Call OpenViking health.
2. Call OpenViking remember to store this single marker fact: "Marker %s has color %s and associated food %s."
3. Call OpenViking find for "%s %s %s".
4. Call OpenViking search for "%s %s %s".
5. Call OpenViking read on the concrete memory URI returned by find or search.
6. Call OpenViking add_resource on this remote URL: %s with to="%s".
7. Call OpenViking add_resource on this local file path: %s with to="%s". The tool should return a signed temp_upload URL. Use shell curl to POST the file bytes to that exact URL as multipart/form-data field "file": curl -fsS -X POST -F "file=@%s" "<signed upload URL>". Do not call add_resource a second time for this local file.
After every tool result succeeds, reply exactly:
CODEX_MCP_RESOURCE_SUBMITTED %s %s %s %s %s`,
		code, markerColor, markerFood,
		code, markerColor, markerFood,
		code, markerColor, markerFood,
		resourceURL,
		resourceURI,
		localResourcePath,
		localResourceURI,
		localResourcePath,
		code, markerColor, markerFood, resourceQuery, localResourceQuery)
}

func codexMCPResourceToolsPrompt(code, markerColor, markerFood, resourceRoot, resourceURI, resourceQuery, localResourceURI, localResourceQuery, grepTerm string) string {
	return fmt.Sprintf(`Use only OpenViking MCP tools for this verification flow. Do not use shell commands, web browsing, or file edits.

The resources have already been submitted and indexed. Execute these steps in order:
1. Call OpenViking search for "%s" scoped to "%s".
2. Call OpenViking read on the concrete resource URI returned by search. If search returns no concrete URI, read "%s".
3. Call OpenViking search for "%s" scoped to "%s".
4. Call OpenViking read on the concrete local resource URI returned by search. If search returns no concrete URI, read "%s".
5. Call OpenViking list on "%s" with recursive=true.
6. Call OpenViking grep for "%s" under "%s".
7. Call OpenViking glob with pattern "**/*.md" under "%s".

After every tool result succeeds, reply exactly:
CODEX_MCP_TOOLS_PASS %s %s %s %s %s`,
		resourceQuery,
		resourceRoot,
		resourceURI,
		localResourceQuery,
		resourceRoot,
		localResourceURI,
		resourceRoot,
		grepTerm,
		resourceRoot,
		resourceRoot,
		code, markerColor, markerFood, resourceQuery, localResourceQuery)
}

func nonce(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
