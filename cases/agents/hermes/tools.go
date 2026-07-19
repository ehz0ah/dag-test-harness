package hermes

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	sharedevidence "code.byted.org/data-arch/ovtest/adapters/evidence"
	hermesadapter "code.byted.org/data-arch/ovtest/adapters/hermes"
	"code.byted.org/data-arch/ovtest/cases/support"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/ops/checks"
	ovops "code.byted.org/data-arch/ovtest/ops/openviking"
	"code.byted.org/data-arch/ovtest/runner"
)

var hermesOpenVikingToolNames = []string{
	"viking_search",
	"viking_read",
	"viking_browse",
	"viking_remember",
	"viking_forget",
	"viking_add_resource",
}

func toolsCase() runner.Case {
	const (
		color = "violet amber"
		food  = "mango sticky rice"
	)
	return runner.Case{
		ID:        "hermes-openviking-tools",
		Goal:      "Hermes completes the native OpenViking memory and resource workflow with semantic postconditions.",
		Reference: "PASS only if structured Hermes state pairs every required viking_* call with a successful tool result, the memory and both resources exist, and the exact remembered URI is then forgotten.",
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(12))
			code := "HTOOL-" + n
			stateID := strings.ToLower(n)
			base := filepath.Join(runner.StateDir(), "hermes", "tools-"+stateID)
			localPath := localResourceFixturePath()
			remoteURL := remoteResourceURL()
			memoryExpect := support.MemoryFactExpect(code, color, food)
			localExpect := []string{"opal-river-409", "indigo silver"}
			remoteExpect := remoteResourceExpect()
			resourceRoot := support.ResourceRoot("hermes", stateID)
			localURI := resourceRoot + "/local-" + strings.ToLower(code) + ".md"
			remoteURI := resourceRoot + "/remote-" + strings.ToLower(code) + ".md"

			user := runner.ConfiguredUser(b, "user")
			workflow := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_tool_workflow",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"home": filepath.Join(base, "workflow"), "toolsets": "memory", "timeout": 900,
					"message": fmt.Sprintf("Use native OpenViking tools only. Remember the note 'Hermes marker %s has color %s and food %s'. Browse the memory root, search for the marker, and read the concrete URI. Add local resource %s with to=%s and wait=true, then add remote resource %s with to=%s and wait=true. Search and read both resources. Reply only WORKFLOW_DONE %s.", code, color, food, localPath, localURI, remoteURL, remoteURI, code),
				}})
			workflowEvidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "tool_workflow_evidence", In: dag.In{"home": workflow.Out("home"), "after": workflow},
				Config: dag.Cfg{"expect_tools": []string{"viking_remember", "viking_browse", "viking_search", "viking_read", "viking_add_resource"}},
			})
			memoryFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_memory_before_forget", In: dag.In{"user_key": user, "after": workflowEvidence},
				Config: dag.Cfg{"query": code + " " + color + " " + food, "expect": memoryExpect, "uri": "viking://user/peers/hermes/memories", "min_results": 1, "settle": 10, "retry": 18,
					"cleanup_kind": "memory", "cleanup_marker": strings.ToLower(code)},
			})
			localFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_local_resource", In: dag.In{"user_key": user, "after": workflowEvidence},
				Config: dag.Cfg{"query": strings.Join(localExpect, " "), "expect": localExpect, "expect_uri": strings.ToLower(localURI), "min_results": 1, "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot)},
			})
			remoteFound := b.Add(ovops.Find, dag.Spec{
				Name: "find_remote_resource", In: dag.In{"user_key": user, "after": workflowEvidence},
				Config: dag.Cfg{"query": strings.Join(remoteExpect, " "), "expect": remoteExpect, "expect_uri": strings.ToLower(remoteURI), "min_results": 1, "settle": 10, "retry": 24,
					"cleanup_kind": "resource", "cleanup_marker": strings.ToLower(resourceRoot)},
			})
			memoryURI := b.Add(sharedevidence.MemoryURI, dag.Spec{
				Name: "exact_memory_uri", In: dag.In{"memories": memoryFound.Out("relevant"), "after": memoryFound},
				Config: dag.Cfg{"uri_prefix": "viking://user/"},
			})
			forget := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_forget_exact_uri",
				In:   dag.In{"user_key": user, "memory_uri": memoryURI.Out("uri"), "after": b.Merge(memoryURI, localFound, remoteFound)},
				Config: dag.Cfg{
					"home": filepath.Join(base, "forget"), "toolsets": "memory",
					"message_template": "Use viking_forget once on this exact URI: {{memory_uri}}. Do not search. Reply only FORGOTTEN.",
				},
			})
			forgetEvidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "forget_evidence", In: dag.In{"home": forget.Out("home"), "after": forget},
				Config: dag.Cfg{"expect_tools": []string{"viking_forget"}},
			})
			gone := b.Add(ovops.URIAbsent, dag.Spec{
				Name: "find_memory_after_forget", In: dag.In{"user_key": user, "uri": memoryURI.Out("uri"), "after": forgetEvidence},
				Config: dag.Cfg{"settle": 5, "retry": 12},
			})
			b.Add(checks.Deterministic, dag.Spec{
				Name: "evidence_check", In: dag.In{"after": b.Merge(workflowEvidence.Out("ok"), memoryFound.Out("ok"), localFound.Out("ok"), remoteFound.Out("ok"), forgetEvidence.Out("ok"), gone.Out("ok"))},
				Config: dag.Cfg{"explanation": "Hermes native tools and deterministic OpenViking postconditions passed"},
			})
		},
	}
}

func prefetchNoToolsCase() runner.Case {
	const color = "cobalt teal"
	return runner.Case{
		ID:   "hermes-openviking-prefetch-no-tools",
		Goal: "Hermes recalls OpenViking-prefetched memory with memory tools disabled.",
		Reference: `PASS only if:
- OpenViking was seeded and indexed before Hermes was asked.
- Hermes ran with a non-memory toolset, and evidence shows no viking_* memory tool call.
- The Hermes reply states both the exact access code and color from the memory.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(2))
			code := "PF-" + n
			stateID := strings.ToLower(n)
			home := filepath.Join(runner.StateDir(), "hermes", "prefetch-no-tools-"+stateID)
			content := fmt.Sprintf("Ariadne's prefetch-only access code is %s, and the marker color is %s.", code, color)
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			seed := b.Add(ovops.AddMemory, dag.Spec{
				Name: "seed_memory", In: dag.In{"user_key": user},
				Config: dag.Cfg{"content": content}})
			indexed := b.Add(ovops.Find, dag.Spec{
				Name: "wait_for_index",
				In:   dag.In{"user_key": user, "after": seed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 12,
				}})
			recall := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_recall",
				In:   dag.In{"user_key": user, "after": indexed},
				Config: dag.Cfg{
					"message":  "What are Ariadne's prefetch-only access code and marker color? Return only the two values.",
					"home":     home,
					"toolsets": []string{},
				}})
			noTools := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "no_memory_tool_evidence",
				In:   dag.In{"home": recall.Out("home"), "after": recall},
				Config: dag.Cfg{
					"forbid": hermesOpenVikingToolNames,
				}})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": recall.Out("reply"), "after": b.Merge(indexed, noTools)},
				Config: dag.Cfg{
					"expect": expect,
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}

func remoteResourceURL() string {
	return support.RemoteResourceURL("")
}

func remoteResourceExpect() []string {
	return support.RemoteResourceExpect("")
}

func localResourceFixturePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("fixtures", "resources", "agent-memory.md")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "fixtures", "resources", "agent-memory.md"))
}

func rememberToolCase() runner.Case {
	const color = "violet amber"
	return runner.Case{
		ID:        "hermes-openviking-tool-remember",
		Goal:      "Hermes writes a memory through the OpenViking remember tool.",
		Reference: `PASS only if Hermes invokes the OpenViking remember capability and OpenViking retrieval finds the stored fact.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(2))
			code := "RM-" + n
			stateID := strings.ToLower(n)
			home := filepath.Join(runner.StateDir(), "hermes", "remember-"+stateID)
			fact := fmt.Sprintf("The Meridian bench note uses ticket %s and ribbon color %s.", code, color)
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			remember := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_remember",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"message":  "Use your OpenViking remember capability to save this long-term note: " + fact + " After it succeeds, reply only STORED.",
					"home":     home,
					"toolsets": "memory",
				}})
			evidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "remember_tool_evidence",
				In:   dag.In{"home": remember.Out("home"), "after": remember},
				Config: dag.Cfg{
					"expect": []string{"viking_remember"},
				}})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_remembered",
				In:   dag.In{"user_key": user, "after": evidence},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 12,
				}})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": remember.Out("reply"), "after": b.Merge(evidence, found)},
				Config: dag.Cfg{
					"expect": []string{"stored"},
				}})
		},
	}
}

func searchReadBrowseToolCase() runner.Case {
	const color = "lapis silver"
	return runner.Case{
		ID:        "hermes-openviking-tool-search-read-browse",
		Goal:      "Hermes uses OpenViking browse, search, and read together to answer from memory.",
		Reference: `PASS only if Hermes invokes OpenViking browse, search, and read; if any is missing, the evidence gate must fail with that tool name.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(2))
			code := "SRB-" + n
			stateID := strings.ToLower(n)
			home := filepath.Join(runner.StateDir(), "hermes", "search-read-browse-"+stateID)
			content := fmt.Sprintf("The Atlas retrieval drill stores beacon %s and trim color %s.", code, color)
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			seed := b.Add(ovops.AddMemory, dag.Spec{
				Name: "seed_memory", In: dag.In{"user_key": user},
				Config: dag.Cfg{"content": content}})
			indexed := b.Add(ovops.Find, dag.Spec{
				Name: "wait_for_index",
				In:   dag.In{"user_key": user, "after": seed},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 12,
				}})
			chat := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_search_read_browse",
				In:   dag.In{"user_key": user, "after": indexed},
				Config: dag.Cfg{
					"message": "Use OpenViking browsing to inspect the memory space, then OpenViking search to locate the Atlas retrieval drill, then OpenViking read on the returned URI. Reply with only the beacon and trim color.",
					"home":    home, "toolsets": "memory",
				}})
			evidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "tool_evidence",
				In:   dag.In{"home": chat.Out("home"), "after": chat},
				Config: dag.Cfg{
					"expect": []string{"viking_browse", "viking_search", "viking_read"},
				}})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": chat.Out("reply"), "after": b.Merge(indexed, evidence)},
				Config: dag.Cfg{
					"expect": expect,
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}

func forgetToolCase() runner.Case {
	const color = "ember white"
	return runner.Case{
		ID:        "hermes-openviking-tool-forget",
		Goal:      "Hermes writes a memory, then uses OpenViking forget to delete the concrete memory URI.",
		Reference: `PASS only if remember writes the fact, retrieval sees it, forget removes it through a concrete URI, and retrieval no longer sees it.`,
		Build: func(b *dag.Builder) {
			n := strings.ToUpper(nonce(2))
			code := "FG-" + n
			stateID := strings.ToLower(n)
			rememberHome := filepath.Join(runner.StateDir(), "hermes", "forget-remember-"+stateID)
			forgetHome := filepath.Join(runner.StateDir(), "hermes", "forget-delete-"+stateID)
			fact := fmt.Sprintf("The forget drill marker is %s and the badge color is %s.", code, color)
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			remember := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_remember",
				In:   dag.In{"user_key": user},
				Config: dag.Cfg{
					"message":  "Use your OpenViking remember capability to save this long-term note: " + fact + " After it succeeds, reply only STORED.",
					"home":     rememberHome,
					"toolsets": "memory",
				}})
			rememberEvidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name:   "remember_evidence",
				In:     dag.In{"home": remember.Out("home"), "after": remember},
				Config: dag.Cfg{"expect": []string{"viking_remember"}}})
			findBefore := b.Add(ovops.Find, dag.Spec{
				Name: "find_before",
				In:   dag.In{"user_key": user, "after": rememberEvidence},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 12,
				}})
			forget := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_forget",
				In:   dag.In{"user_key": user, "after": findBefore},
				Config: dag.Cfg{
					"message": "Use OpenViking search to find the memory about forget drill marker " + code + ". Then use OpenViking forget on the exact concrete URI returned by search. Reply only FORGOTTEN.",
					"home":    forgetHome, "toolsets": "memory",
				}})
			forgetEvidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name:   "forget_evidence",
				In:     dag.In{"home": forget.Out("home"), "after": forget},
				Config: dag.Cfg{"expect": []string{"viking_search", "viking_forget"}}})
			findAfter := b.Add(ovops.Find, dag.Spec{
				Name: "find_after",
				In:   dag.In{"user_key": user, "after": forgetEvidence},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"expect_gone": true, "min_results": 1,
					"settle": 10, "retry": 12,
				}})
			b.Add(checks.Deterministic, dag.Spec{Name: "check",
				In: dag.In{"after": findAfter},
				Config: dag.Cfg{
					"explanation": "remembered OpenViking memory was forgotten and no longer appears in retrieval",
				}})
		},
	}
}

func addLocalResourceToolCase() runner.Case {
	return runner.Case{
		ID:        "hermes-openviking-tool-add-local-resource",
		Goal:      "Hermes ingests a local file through OpenViking add resource and retrieves it.",
		Reference: `PASS only if Hermes invokes the add-resource tool, OpenViking indexes the local file, and Hermes answers from the indexed content.`,
		Build: func(b *dag.Builder) {
			stateID := strings.ToLower(nonce(2))
			home := filepath.Join(runner.StateDir(), "hermes", "local-resource-"+stateID)
			filePath := localResourceFixturePath()
			code := "opal-river-409"
			color := "indigo silver"
			expect := []string{strings.ToLower(code), color}

			user := runner.ConfiguredUser(b, "user")
			resource := b.Var(filePath, "local_resource")
			chat := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_add_resource",
				In:   dag.In{"user_key": user, "resource_path": resource.Out("value"), "after": resource},
				Config: dag.Cfg{
					"message_template": "Use OpenViking add-resource on this local file path: {{resource_path}}. Set wait to true and timeout to 180 seconds. After it is indexed, search and read the resource, then reply with only the access phrase and checksum color.",
					"home":             home,
					"toolsets":         "memory",
					"timeout":          360,
				}})
			evidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "resource_tool_evidence",
				In:   dag.In{"home": chat.Out("home"), "after": chat},
				Config: dag.Cfg{
					"expect": []string{"viking_add_resource"},
				}})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_resource",
				In:   dag.In{"user_key": user, "after": evidence},
				Config: dag.Cfg{
					"query": code + " " + color, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 18,
				}})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": chat.Out("reply"), "after": b.Merge(evidence, found)},
				Config: dag.Cfg{
					"expect": expect,
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}

func addRemoteResourceToolCase() runner.Case {
	return runner.Case{
		ID:        "hermes-openviking-tool-add-remote-resource",
		Goal:      "Hermes ingests a remote URL through OpenViking add resource and retrieves it.",
		Reference: `PASS only if Hermes invokes the add-resource tool, OpenViking indexes the remote URL, and Hermes answers from the indexed content.`,
		Build: func(b *dag.Builder) {
			url := remoteResourceURL()
			expect := remoteResourceExpect()
			stateID := strings.ToLower(nonce(2))
			home := filepath.Join(runner.StateDir(), "hermes", "remote-resource-"+stateID)
			query := strings.Join(expect, " ")

			user := runner.ConfiguredUser(b, "user")
			remote := b.Var(url, "remote_resource")
			chat := b.Add(hermesadapter.Chat, dag.Spec{
				Name: "chat_add_resource",
				In:   dag.In{"user_key": user, "resource_url": remote.Out("value"), "after": remote},
				Config: dag.Cfg{
					"message_template": "Use OpenViking add-resource on this remote URL: {{resource_url}}. Set wait to true and timeout to 180 seconds. After it is indexed, search and read the resource, then reply only with these expected token(s): " + query + ".",
					"home":             home,
					"toolsets":         "memory",
					"timeout":          360,
				}})
			evidence := b.Add(hermesadapter.Evidence, dag.Spec{
				Name: "resource_tool_evidence",
				In:   dag.In{"home": chat.Out("home"), "after": chat},
				Config: dag.Cfg{
					"expect": []string{"viking_add_resource"},
				}})
			found := b.Add(ovops.Find, dag.Spec{
				Name: "find_resource",
				In:   dag.In{"user_key": user, "after": evidence},
				Config: dag.Cfg{
					"query": query, "expect": expect,
					"min_results": 1, "settle": 10, "retry": 18,
				}})
			b.Add(checks.Text, dag.Spec{Name: "reply_check",
				In: dag.In{"text": chat.Out("reply"), "after": b.Merge(evidence, found)},
				Config: dag.Cfg{
					"expect": expect,
					"forbid": []string{"i do not know", "unknown", "not sure"},
				}})
		},
	}
}
