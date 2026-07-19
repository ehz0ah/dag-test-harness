package ops

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
)

// ops: the typed ov operator library. One op per CLI capability with an intrinsic
// deterministic gate; semantic params go in config (each op validates the keys it
// reads via need), dynamic data (user_key, session_id, uri, memories) flows as
// typed inputs. New capability = a new factory value; readiness polling lives in
// the op via base.poll.

func terminal(factory dag.Factory) dag.Factory {
	factory.Terminal = true
	return factory
}

var (
	OvDeleteAccount      = deleteAccountOp()
	OvCreateAccount      = createAccountOp()
	TextCheck            = terminal(textCheckOp())
	OvCommand            = commandOp()
	OvCheck              = terminal(checkOp())
	OvAddMemory          = addMemoryOp()
	OvWait               = waitOp()
	OvList               = lsOp()
	OvFind               = findOp()
	OvSearch             = searchOp()
	OvURIAbsent          = uriAbsentOp()
	OvRemove             = rmOp()
	OvSessionNew         = sessionNewOp()
	OvSessionAddMessage  = sessionAddMessageOp()
	OvSessionAddMessages = sessionAddMessagesOp()
	OvSessionCommit      = sessionCommitOp()
	OvSessionPresent     = sessionPresentOp()
	OvSessionCommitted   = sessionCommittedOp()
	FixtureFile          = fixtureFileOp()
	FixtureServer        = fixtureServerOp()
	OvJudge              = terminal(judgeOp())
)

// ── generic CLI/check ops ──────────────────────────────────────────────────--

func textCheckOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"text", "after"}, Outputs: []string{"ok", "text", "verdict"}}, true, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			text := asString(in["text"])
			if text == "" {
				return nil, gateErr(b.name, "text_check requires non-empty input text")
			}
			haystack := strings.ToLower(text)
			if missing := missingTokens(haystack, lowerAll(tokenList(b.oc["expect"]))); len(missing) > 0 {
				return nil, gateErr(b.name, fmt.Sprintf("text missing expected token(s) %v", missing))
			}
			alternatives := lowerAll(tokenList(b.oc["expect_any"]))
			if len(alternatives) > 0 && len(presentTokens(haystack, alternatives)) == 0 {
				return nil, gateErr(b.name, fmt.Sprintf("text missing every accepted alternative %v", alternatives))
			}
			if leaked := presentTokens(haystack, lowerAll(tokenList(b.oc["forbid"]))); len(leaked) > 0 {
				return nil, gateErr(b.name, fmt.Sprintf("text contained forbidden token(s) %v", leaked))
			}
			return map[string]any{
				"ok":      true,
				"text":    text,
				"verdict": map[string]any{"pass": true, "explanation": "text check passed"},
			}, nil
		}
	})
}

func commandOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"ok", "result", "text"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			rawArgs, err := b.need("args")
			if err != nil {
				return nil, err
			}
			args := asStrings(rawArgs)
			if len(args) == 0 {
				return nil, configErr(b.name, "ov_command requires non-empty args")
			}

			settle := asInt(b.oc["settle"], 0)
			var r cliResult
			if asBool(b.oc["admin"]) {
				r, err = b.runAdmin(args, settle)
			} else {
				r, err = b.runAs(in["user_key"], args, settle)
			}
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			res, parseErr := resultOf(r.Stdout)
			if parseErr != nil {
				return nil, gateErr(b.name, "command output not JSON: "+parseErr.Error())
			}
			haystack := strings.ToLower(r.Stdout + "\n" + jsonDump(res))
			if missing := missingTokens(haystack, lowerAll(asStrings(b.oc["expect"]))); len(missing) > 0 {
				return nil, gateErr(b.name, fmt.Sprintf("output missing expected token(s) %v", missing))
			}
			if leaked := presentTokens(haystack, lowerAll(asStrings(b.oc["forbid"]))); len(leaked) > 0 {
				return nil, gateErr(b.name, fmt.Sprintf("output contained forbidden token(s) %v", leaked))
			}
			if minCount := asInt(b.oc["min_count"], -1); minCount >= 0 && resultCount(res) < minCount {
				return nil, gateErr(b.name, fmt.Sprintf("result count %d < %d", resultCount(res), minCount))
			}
			out := CLIFields(r)
			out["ok"], out["result"], out["text"], out["count"] = true, res, r.Stdout, resultCount(res)
			return out, nil
		}
	})
}

func checkOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"after"}, Outputs: []string{"verdict"}}, true, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			if !allTruthy(in["after"]) {
				return nil, gateErr(b.name, "deterministic check input was false")
			}
			explanation := firstNonEmpty(asString(b.oc["explanation"]), "deterministic checks passed")
			return map[string]any{"verdict": map[string]any{"pass": true, "explanation": explanation}}, nil
		}
	})
}

// ── admin ops ───────────────────────────────────────────────────────────────--

func deleteAccountOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"after"}, Outputs: []string{"ok"}}, false, func(b *base) execFn {
		return func(map[string]any) (map[string]any, error) {
			account, err := b.needStr("account")
			if err != nil {
				return nil, err
			}
			r, err := b.runAdmin([]string{"admin", "delete-account", account}, 0)
			if err != nil {
				return nil, err
			}
			out := CLIFields(r)
			out["ok"] = r.ExitCode == 0
			return out, nil
		}
	})
}

func createAccountOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"after"}, Outputs: []string{"user_key"}}, true, func(b *base) execFn {
		return func(map[string]any) (map[string]any, error) {
			account, err := b.needStr("account")
			if err != nil {
				return nil, err
			}
			admin, err := b.needStr("admin_user")
			if err != nil {
				return nil, err
			}
			r, err := b.runAdmin([]string{"admin", "create-account", "--admin", admin, account}, 0)
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			res, err := b.jsonResult(r, "create")
			if err != nil {
				return nil, err
			}
			key := asString(res["user_key"])
			if key == "" {
				return nil, gateErr(b.name, "no user_key in: "+truncate(strings.TrimSpace(r.Stdout), 200))
			}
			out := CLIFields(r)
			out["user_key"] = key
			out["account_id"] = res["account_id"]
			out["admin_user_id"] = res["admin_user_id"]
			return out, nil
		}
	})
}

// ── user (data) ops ─────────────────────────────────────────────────────────--

func addMemoryOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"ok"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			content, err := b.needStr("content")
			if err != nil {
				return nil, err
			}
			r, err := b.runAs(in["user_key"], []string{"add-memory", content}, 0)
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			out := CLIFields(r)
			out["ok"] = true
			return out, nil
		}
	})
}

func waitOp() dag.Factory {
	// SOFT by construction: records status, never raises (no ok() gate).
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"ok"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			timeout := asInt(b.oc["timeout"], 120)
			r, err := b.runAs(in["user_key"], []string{"wait", "--timeout", strconv.Itoa(timeout)}, 0)
			if err != nil {
				return nil, err
			}
			processed, _ := resultOf(r.Stdout)
			out := CLIFields(r)
			out["ok"] = r.ExitCode == 0
			out["processed"] = processed
			return out, nil
		}
	})
}

func lsOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"entries"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			r, err := b.runAs(in["user_key"], []string{"ls"}, 0)
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			entries := []any{}
			var parseErr any
			res, e := resultOf(r.Stdout)
			if e != nil {
				parseErr = "ls JSON parse failed: " + e.Error()
			} else if lst, ok := res.([]any); ok {
				for _, item := range lst {
					if m, ok := item.(map[string]any); ok {
						entries = append(entries, m["uri"])
					}
				}
			}
			out := CLIFields(r)
			out["entries"] = entries
			out["parse_error"] = parseErr
			return out, nil
		}
	})
}

func findOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"memories", "relevant", "ok", CleanupClaimsOutput}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			query, err := b.needStr("query")
			if err != nil {
				return nil, err
			}
			argv := []string{"find", query}
			if uri := asString(b.oc["uri"]); uri != "" {
				argv = append(argv, "-u", uri)
			}
			minResults := asInt(b.oc["min_results"], 1)
			expectURI := strings.ToLower(asString(b.oc["expect_uri"]))
			expect := lowerAll(asStrings(b.oc["expect"]))
			forbid := lowerAll(asStrings(b.oc["forbid"]))
			expectGone := asBool(b.oc["expect_gone"])
			settle, retry := asInt(b.oc["settle"], 0), asInt(b.oc["retry"], 0)
			conf, err := userConf(b.name, in["user_key"])
			if err != nil {
				return nil, err
			}

			relevant := func(mems []map[string]any) []map[string]any {
				out := mems
				if expectURI != "" {
					out = filterMems(out, func(m map[string]any) bool {
						return strings.Contains(strings.ToLower(asString(m["uri"])), expectURI)
					})
				}
				if len(expect) > 0 {
					out = filterMems(out, func(m map[string]any) bool {
						return containsAll(strings.ToLower(asString(m["abstract"])), expect)
					})
				}
				return out
			}

			attempt := func(last bool) (map[string]any, error) {
				r := runOvContext(b.context(), argv, conf, settle)
				if r.ExitCode != 0 {
					if last {
						return nil, gateErr(b.name, ExitDetail(r))
					}
					return nil, nil
				}
				mems, parseErr := memoriesOf(r.Stdout)
				if parseErr != "" {
					if last {
						return nil, gateErr(b.name, parseErr)
					}
					return nil, nil
				}
				if len(forbid) > 0 {
					leakedSet := map[string]bool{}
					for _, m := range mems {
						abs := strings.ToLower(asString(m["abstract"]))
						for _, t := range forbid {
							if strings.Contains(abs, t) {
								leakedSet[t] = true
							}
						}
					}
					if len(leakedSet) > 0 {
						leaked := sortedKeys(leakedSet)
						return nil, gateErr(b.name, fmt.Sprintf(
							"forbidden token(s) %v leaked into a returned abstract "+
								"(a memory that should never have existed)", leaked))
					}
				}
				rel := relevant(mems)
				ready := len(rel) >= minResults
				if expectGone {
					ready = len(rel) == 0
				}
				if ready {
					out := CLIFields(r)
					out["ok"], out["memories"], out["relevant"], out["count"] = true, withSourceNode(mems, b.name), withSourceNode(rel, b.name), len(mems)
					out["parse_error"] = emptyToNil(parseErr)
					claims, claimErr := relevantCleanupClaims(b.name, b.oc, rel)
					if claimErr != nil {
						return nil, claimErr
					}
					out[CleanupClaimsOutput] = claims
					return out, nil
				}
				if last {
					if expectGone {
						return nil, gateErr(b.name, fmt.Sprintf(
							"memory matching uri~%q abstract~%v is STILL present (%d) after %d attempts — ghost resurfacing",
							asString(b.oc["expect_uri"]), b.oc["expect"], len(rel), retry+1))
					}
					hint := ""
					if parseErr != "" {
						hint = "; parse_error: " + parseErr
					}
					if sample := sampleMemories(mems, 3); sample != "" {
						hint += "; sample: " + sample
					}
					return nil, gateErr(b.name, fmt.Sprintf(
						"no memory matching uri~%q abstract~%v after %d attempts (%d memories indexed%s)",
						asString(b.oc["expect_uri"]), b.oc["expect"], retry+1, len(mems), hint))
				}
				return nil, nil
			}

			out, attempts, err := b.poll(attempt, retry)
			if err != nil {
				return nil, err
			}
			out["attempts"] = attempts
			return out, nil
		}
	})
}

func relevantCleanupClaims(node string, cfg map[string]any, relevant []map[string]any) ([]CleanupClaim, error) {
	kind := strings.TrimSpace(asString(cfg["cleanup_kind"]))
	if kind == "" {
		return nil, nil
	}
	if kind != "memory" && kind != "resource" {
		return nil, configErr(node, "cleanup_kind must be memory or resource")
	}
	marker := strings.ToLower(strings.TrimSpace(asString(cfg["cleanup_marker"])))
	if marker == "" {
		return nil, configErr(node, "cleanup_marker is required when cleanup_kind is set")
	}
	claims := make([]CleanupClaim, 0, len(relevant))
	for _, item := range relevant {
		uri := strings.TrimSpace(asString(item["uri"]))
		evidence := strings.ToLower(uri + "\n" + asString(item["abstract"]) + "\n" + asString(item["overview"]))
		if !ValidCleanupClaimURI(uri, kind) {
			return nil, gateErr(node, fmt.Sprintf("verified cleanup candidate %q is not an exact %s result URI", uri, kind))
		}
		if !strings.Contains(evidence, marker) {
			return nil, gateErr(node, fmt.Sprintf("verified cleanup candidate %q lacks %s scope marker %q", uri, kind, marker))
		}
		claims = append(claims, CleanupClaim{
			URI: uri, Kind: kind,
			Proof: "exact relevant OpenViking result matched the run-scoped marker",
		})
	}
	return claims, nil
}

// ValidCleanupClaimURI recognizes exact leaf shapes returned by OpenViking.
// User memories can be personal or peer-scoped; namespace roots are rejected.
func ValidCleanupClaimURI(rawURI, kind string) bool {
	uri, err := url.Parse(rawURI)
	if err != nil || uri.Scheme != "viking" || uri.RawQuery != "" || uri.Fragment != "" || uri.User != nil {
		return false
	}
	segments := strings.FieldsFunc(uri.EscapedPath(), func(r rune) bool { return r == '/' })
	if kind == "resource" {
		return uri.Host == "resources" && len(segments) >= 2
	}
	if kind != "memory" || (uri.Host != "user" && uri.Host != "agent") {
		return false
	}
	personal := len(segments) >= 3 && segments[1] == "memories"
	peer := uri.Host == "user" && len(segments) >= 5 && segments[1] == "peers" && segments[3] == "memories"
	return personal || peer
}

func searchOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"memories"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			query, err := b.needStr("query")
			if err != nil {
				return nil, err
			}
			r, err := b.runAs(in["user_key"], []string{"search", query}, asInt(b.oc["settle"], 0))
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			mems, parseErr := memoriesOf(r.Stdout)
			if parseErr != "" {
				return nil, gateErr(b.name, parseErr)
			}
			if len(mems) < asInt(b.oc["min_results"], 1) {
				return nil, gateErr(b.name, fmt.Sprintf("search returned %d memories", len(mems)))
			}
			out := CLIFields(r)
			out["memories"], out["count"], out["parse_error"] = withSourceNode(mems, b.name), len(mems), emptyToNil(parseErr)
			return out, nil
		}
	})
}

func uriAbsentOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "uri", "after"}, Outputs: []string{"ok", "uri", "attempts"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			uri := strings.TrimSpace(asString(in["uri"]))
			if uri == "" {
				return nil, gateErr(b.name, "uri_absent requires non-empty input uri")
			}
			settle, retry := asInt(b.oc["settle"], 5), asInt(b.oc["retry"], 12)
			conf, err := userConf(b.name, in["user_key"])
			if err != nil {
				return nil, err
			}
			attempt := func(last bool) (map[string]any, error) {
				r := runOvContext(b.context(), []string{"read", uri}, conf, settle)
				if r.ExitCode != 0 {
					detail := ExitDetail(r)
					lower := strings.ToLower(detail)
					if strings.Contains(lower, "not_found") || strings.Contains(lower, "file not found") {
						out := CLIFields(r)
						out["ok"], out["uri"], out["detail"] = true, uri, detail
						return out, nil
					}
					return nil, gateErr(b.name, detail)
				}
				if last {
					return nil, gateErr(b.name, fmt.Sprintf("exact URI %s is still readable after %d attempts", uri, retry+1))
				}
				return nil, nil
			}
			out, attempts, err := b.poll(attempt, retry)
			if err != nil {
				return nil, err
			}
			out["attempts"] = attempts
			return out, nil
		}
	})
}

func rmOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "memories", "after"}, Outputs: []string{"removed_uri", "removed_uris"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			uri := asString(b.oc["uri"])
			uris := []string{}
			if uri == "" {
				uf := strings.ToLower(asString(b.oc["uri_filter"]))
				af := strings.ToLower(asString(b.oc["abstract_filter"]))
				for _, m := range asMemList(in["memories"]) {
					if (uf != "" && strings.Contains(strings.ToLower(asString(m["uri"])), uf)) ||
						(af != "" && strings.Contains(strings.ToLower(asString(m["abstract"])), af)) {
						if u := asString(m["uri"]); u != "" && !containsString(uris, u) {
							uris = append(uris, u)
						}
						if !asBool(b.oc["all_matches"]) {
							break
						}
					}
				}
				if len(uris) == 0 {
					return nil, gateErr(b.name, fmt.Sprintf(
						"no uri to remove (config 'uri' unset and no wired memory matched uri_filter~%q / abstract_filter~%q)", uf, af))
				}
			} else {
				uris = append(uris, uri)
			}
			var r cliResult
			for _, u := range uris {
				argv := []string{"rm", u}
				if asBool(b.oc["recursive"]) {
					argv = append(argv, "--recursive")
				}
				var err error
				r, err = b.runAs(in["user_key"], argv, 0)
				if err != nil {
					return nil, err
				}
				if err := b.ok(r); err != nil {
					return nil, err
				}
			}
			out := CLIFields(r)
			out["removed_uri"], out["removed_uris"], out["ok"] = uris[0], uris, true
			return out, nil
		}
	})
}

// ── ov-native session ops ───────────────────────────────────────────────────--

func sessionNewOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "after"}, Outputs: []string{"session_id", "uri", CleanupClaimsOutput}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			if policy := b.oc["memory_policy"]; policy != nil {
				r, err := runAsAPI(b.name, in["user_key"], http.MethodPost, "/api/v1/sessions",
					map[string]any{"memory_policy": policy}, 0)
				if err != nil {
					return nil, err
				}
				if err := b.ok(r); err != nil {
					return nil, err
				}
				res, err := b.jsonResult(r, "session new")
				if err != nil {
					return nil, err
				}
				sid := asString(res["session_id"])
				if sid == "" {
					return nil, gateErr(b.name, "no session_id in: "+truncate(strings.TrimSpace(r.Stdout), 200))
				}
				out := CLIFields(r)
				out["session_id"], out["uri"] = sid, res["uri"]
				if uri := asString(res["uri"]); uri != "" {
					out[CleanupClaimsOutput] = []CleanupClaim{{URI: uri, Kind: "session", Proof: "session create response returned the exact URI"}}
				}
				return out, nil
			}
			r, err := b.runAs(in["user_key"], []string{"session", "new"}, 0)
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			res, err := b.jsonResult(r, "session new")
			if err != nil {
				return nil, err
			}
			sid := asString(res["session_id"])
			if sid == "" {
				return nil, gateErr(b.name, "no session_id in: "+truncate(strings.TrimSpace(r.Stdout), 200))
			}
			out := CLIFields(r)
			out["session_id"], out["uri"] = sid, res["uri"]
			if uri := asString(res["uri"]); uri != "" {
				out[CleanupClaimsOutput] = []CleanupClaim{{URI: uri, Kind: "session", Proof: "session create response returned the exact URI"}}
			}
			return out, nil
		}
	})
}

func sessionAddMessagesOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "session_id", "messages", "after"},
		Outputs: []string{"ok", "added", "message_count"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			sid := asString(in["session_id"])
			if sid == "" {
				return nil, gateErr(b.name, "missing session_id (wire it from ov_session_new)")
			}
			messages, ok := asMapList(in["messages"])
			if !ok {
				return nil, gateErr(b.name, fmt.Sprintf("messages must be a list of objects, got %T", in["messages"]))
			}
			if len(messages) == 0 {
				return nil, gateErr(b.name, "messages must not be empty")
			}
			r, err := runAsAPI(b.name, in["user_key"], http.MethodPost,
				"/api/v1/sessions/"+sid+"/messages/batch", map[string]any{"messages": messages}, 0)
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			res, err := b.jsonResult(r, "batch add-message")
			if err != nil {
				return nil, err
			}
			added := res["added"]
			count := res["message_count"]
			if asInt(added, -1) != len(messages) {
				return nil, gateErr(b.name, fmt.Sprintf("added %v != input messages %d", added, len(messages)))
			}
			out := CLIFields(r)
			out["ok"], out["added"], out["message_count"] = true, added, count
			return out, nil
		}
	})
}

func sessionAddMessageOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "session_id", "after"}, Outputs: []string{"ok"}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			sid := asString(in["session_id"])
			if sid == "" {
				return nil, gateErr(b.name, "missing session_id (wire it from ov_session_new)")
			}
			content, err := b.needStr("content")
			if err != nil {
				return nil, err
			}
			role := asString(b.oc["role"])
			if role == "" {
				role = "user"
			}
			r, err := b.runAs(in["user_key"],
				[]string{"session", "add-message", sid, "--role", role, "--content", content}, 0)
			if err != nil {
				return nil, err
			}
			if err := b.ok(r); err != nil {
				return nil, err
			}
			res, err := b.jsonResult(r, "add-message")
			if err != nil {
				return nil, err
			}
			count := res["message_count"]
			if ec, ok := b.oc["expect_count"]; ok && ec != nil {
				if asInt(count, -1) != asInt(ec, -2) {
					return nil, gateErr(b.name, fmt.Sprintf(
						"message_count %v != expected transcript position %v", count, ec))
				}
			}
			out := CLIFields(r)
			out["ok"], out["message_count"] = true, count
			return out, nil
		}
	})
}

func sessionCommitOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "session_id", "after"}, Outputs: []string{"ok", CleanupClaimsOutput}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			sid := asString(in["session_id"])
			if sid == "" {
				return nil, gateErr(b.name, "missing session_id (wire it from ov_session_new)")
			}
			conf, err := userConf(b.name, in["user_key"])
			if err != nil {
				return nil, err
			}
			r := runOvContext(b.context(), []string{"session", "commit", sid}, conf, 0)
			if err := b.ok(r); err != nil {
				return nil, err
			}
			res, err := b.jsonResult(r, "commit")
			if err != nil {
				return nil, err
			}
			if asString(res["status"]) != "accepted" || !asBool(res["archived"]) || asString(res["task_id"]) == "" {
				return nil, gateErr(b.name, "commit not accepted/archived: "+truncate(jsonStr(res), 200))
			}
			minExtracted := asInt(b.oc["min_extracted"], 1)
			settle, retry := asInt(b.oc["settle"], 5), asInt(b.oc["retry"], 12)
			taskID := asString(res["task_id"])

			attempt := func(last bool) (map[string]any, error) {
				g := runOvContext(b.context(), []string{"task", "status", taskID}, conf, settle)
				if g.ExitCode != 0 {
					if last {
						return nil, gateErr(b.name, "commit task status failed: "+ExitDetail(g))
					}
					return nil, nil
				}
				rawTask, parseErr := resultOf(g.Stdout)
				if parseErr != nil {
					if last {
						return nil, gateErr(b.name, "commit task status output not JSON: "+parseErr.Error())
					}
					return nil, nil
				}
				task, ok := rawTask.(map[string]any)
				if !ok {
					if last {
						return nil, gateErr(b.name, fmt.Sprintf("commit task status result is not an object: %T", rawTask))
					}
					return nil, nil
				}
				switch asString(task["status"]) {
				case "failed":
					return nil, gateErr(b.name, "commit task failed: "+truncate(asString(task["error"]), 300))
				case "completed":
					payload, ok := task["result"].(map[string]any)
					if !ok {
						return nil, gateErr(b.name, fmt.Sprintf("completed commit task result is not an object: %T", task["result"]))
					}
					extracted, _ := payload["memories_extracted"].(map[string]any)
					total := memoryCountTotal(extracted)
					if total < minExtracted {
						return nil, gateErr(b.name, fmt.Sprintf(
							"commit task completed with %d extracted memories, want at least %d (%v)",
							total, minExtracted, emptyMapToNil(extracted)))
					}
					extracted = cloneMap(extracted)
					extracted["total"] = total
					out := CLIFields(g)
					out["ok"], out["extracted"] = true, extracted
					out["task_id"], out["archive_uri"] = taskID, res["archive_uri"]
					out[CleanupClaimsOutput] = []CleanupClaim{}
					if asBool(b.oc["cleanup_added_memories"]) {
						claims, err := commitAddedMemoryClaims(b, conf, payload)
						if err != nil {
							return nil, err
						}
						out[CleanupClaimsOutput] = claims
					}
					return out, nil
				}
				if last {
					return nil, gateErr(b.name, fmt.Sprintf(
						"commit task %s not complete after %d polls (status=%q)",
						taskID, retry+1, asString(task["status"])))
				}
				return nil, nil
			}

			out, attempts, err := b.poll(attempt, retry)
			if err != nil {
				return nil, err
			}
			out["attempts"] = attempts
			return out, nil
		}
	})
}

func commitAddedMemoryClaims(b *base, conf string, payload map[string]any) ([]CleanupClaim, error) {
	uri := strings.TrimSpace(asString(payload["memory_diff_uri"]))
	if uri == "" {
		return nil, gateErr(b.name, "completed commit task omitted memory_diff_uri required for exact cleanup")
	}
	r := runOvContext(b.context(), []string{"read", uri}, conf, 0)
	if err := b.ok(r); err != nil {
		return nil, err
	}
	raw, err := resultOf(r.Stdout)
	if err != nil {
		return nil, gateErr(b.name, "memory diff read output not JSON: "+err.Error())
	}
	encoded, ok := raw.(string)
	if !ok {
		return nil, gateErr(b.name, fmt.Sprintf("memory diff read result is not a string: %T", raw))
	}
	var diff struct {
		Operations struct {
			Adds []struct {
				URI string `json:"uri"`
			} `json:"adds"`
		} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(encoded), &diff); err != nil {
		return nil, gateErr(b.name, "memory diff content not JSON: "+err.Error())
	}
	claims := make([]CleanupClaim, 0, len(diff.Operations.Adds))
	for _, add := range diff.Operations.Adds {
		if !ValidCleanupClaimURI(add.URI, "memory") {
			return nil, gateErr(b.name, fmt.Sprintf("memory diff add %q is not an exact memory URI", add.URI))
		}
		claims = append(claims, CleanupClaim{
			URI: add.URI, Kind: "memory",
			Proof: "commit memory_diff listed the exact added memory URI",
		})
	}
	return claims, nil
}

func sessionPresentOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "session_id", "after"},
		Outputs: []string{"ok", "info", "uri", CleanupClaimsOutput}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			sid := strings.TrimSpace(asString(in["session_id"]))
			if sid == "" {
				return nil, gateErr(b.name, "missing session_id")
			}
			conf, err := userConf(b.name, in["user_key"])
			if err != nil {
				return nil, err
			}
			g := runOvContext(b.context(), []string{"session", "get", sid}, conf, asInt(b.oc["settle"], 0))
			if err := b.ok(g); err != nil {
				return nil, err
			}
			info, err := sessionInfoFromGet(g.Stdout)
			if err != nil {
				return nil, gateErr(b.name, err.Error())
			}
			uri := asString(info["uri"])
			if uri == "" {
				return nil, gateErr(b.name, "session get omitted the canonical session URI")
			}
			out := CLIFields(g)
			out["ok"], out["info"], out["uri"] = true, info, uri
			out[CleanupClaimsOutput] = []CleanupClaim{{
				URI: uri, Kind: "session", Proof: "session get confirmed the exact existing session",
			}}
			return out, nil
		}
	})
}

func sessionCommittedOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"user_key", "session_id", "after"},
		Outputs: []string{"ok", "info", "extracted", "uri", CleanupClaimsOutput}}, false, func(b *base) execFn {
		return func(in map[string]any) (map[string]any, error) {
			sid := asString(in["session_id"])
			if sid == "" {
				return nil, gateErr(b.name, "missing session_id (wire it from hermes_chat)")
			}
			conf, err := userConf(b.name, in["user_key"])
			if err != nil {
				return nil, err
			}
			minCommits := asInt(b.oc["min_commits"], 1)
			minExtracted := asInt(b.oc["min_extracted"], 1)
			settle, retry := asInt(b.oc["settle"], 5), asInt(b.oc["retry"], 12)
			if asBool(b.oc["poll_commit_task"]) {
				return pollPluginSessionCommit(b, conf, sid, minCommits, minExtracted, settle, retry)
			}

			attempt := func(last bool) (map[string]any, error) {
				g := runOvContext(b.context(), []string{"session", "get", sid}, conf, settle)
				if g.ExitCode != 0 {
					if last {
						return nil, gateErr(b.name, "session get failed: "+ExitDetail(g))
					}
					return nil, nil
				}
				info, parseErr := sessionInfoFromGet(g.Stdout)
				if parseErr != nil {
					if last {
						return nil, gateErr(b.name, parseErr.Error())
					}
					return nil, nil
				}
				extracted, ready := sessionCommittedReady(info, minCommits, minExtracted)
				if ready {
					uri := asString(info["uri"])
					if uri == "" {
						return nil, gateErr(b.name, "session get omitted the canonical session URI")
					}
					out := CLIFields(g)
					out["ok"], out["info"], out["extracted"] = true, info, extracted
					out["session_id"], out["uri"], out["commit_count"], out["archive_uri"] = sid, uri, asInt(info["commit_count"], 0), info["archive_uri"]
					out[CleanupClaimsOutput] = []CleanupClaim{{
						URI: uri, Kind: "session",
						Proof: "session get confirmed the exact committed and extracted session",
					}}
					return out, nil
				}
				if last {
					return nil, gateErr(b.name, fmt.Sprintf(
						"session not committed/extracted after %d polls (commit_count=%v, min_commits=%d, extracted=%v, min_extracted=%d)",
						retry+1, info["commit_count"], minCommits, emptyMapToNil(extracted), minExtracted))
				}
				return nil, nil
			}

			out, attempts, err := b.poll(attempt, retry)
			if err != nil {
				return nil, err
			}
			out["attempts"] = attempts
			return out, nil
		}
	})
}

func pollPluginSessionCommit(b *base, conf, sid string, minCommits, minExtracted, settle, retry int) (map[string]any, error) {
	taskID := ""
	attempt := func(last bool) (map[string]any, error) {
		discoveredNow := false
		if taskID == "" {
			listed := runOvContext(b.context(), []string{"task", "list", "--task-type", "session_commit"}, conf, settle)
			if listed.ExitCode != 0 {
				if last {
					return nil, gateErr(b.name, "session commit task discovery failed: "+ExitDetail(listed))
				}
				return nil, nil
			}
			raw, err := resultOf(listed.Stdout)
			if err != nil {
				if last {
					return nil, gateErr(b.name, "session commit task list output not JSON: "+err.Error())
				}
				return nil, nil
			}
			task := latestSessionCommitTask(asMemList(raw), sid)
			taskID = strings.TrimSpace(asString(task["task_id"]))
			if taskID == "" {
				if last {
					return nil, gateErr(b.name, fmt.Sprintf("no session_commit task found for session %s after %d polls", sid, retry+1))
				}
				return nil, nil
			}
			discoveredNow = true
		}

		statusSettle := settle
		if discoveredNow {
			// Discovery already waited for this poll interval.
			statusSettle = 0
		}
		statusResult := runOvContext(b.context(), []string{"task", "status", taskID}, conf, statusSettle)
		if statusResult.ExitCode != 0 {
			if last {
				return nil, gateErr(b.name, "session commit task status failed: "+ExitDetail(statusResult))
			}
			return nil, nil
		}
		raw, err := resultOf(statusResult.Stdout)
		if err != nil {
			if last {
				return nil, gateErr(b.name, "session commit task status output not JSON: "+err.Error())
			}
			return nil, nil
		}
		task, ok := raw.(map[string]any)
		if !ok {
			if last {
				return nil, gateErr(b.name, fmt.Sprintf("session commit task status result is not an object: %T", raw))
			}
			return nil, nil
		}

		switch asString(task["status"]) {
		case "failed":
			return nil, gateErr(b.name, "session commit task failed: "+truncate(asString(task["error"]), 300))
		case "completed":
			payloads, complete, err := completedSessionCommitPayloads(b, conf, sid, last)
			if err != nil {
				return nil, err
			}
			if !complete {
				return nil, nil
			}

			sessionResult := runOvContext(b.context(), []string{"session", "get", sid}, conf, 0)
			if sessionResult.ExitCode != 0 {
				return nil, gateErr(b.name, "committed session get failed: "+ExitDetail(sessionResult))
			}
			info, err := sessionInfoFromGet(sessionResult.Stdout)
			if err != nil {
				return nil, gateErr(b.name, err.Error())
			}
			uri := strings.TrimSpace(asString(info["uri"]))
			if uri == "" {
				return nil, gateErr(b.name, "session get omitted the canonical session URI")
			}
			commitCount := asInt(info["commit_count"], 0)
			if commitCount < minCommits {
				return nil, gateErr(b.name, fmt.Sprintf("session commit_count %d is below required %d after task completion", commitCount, minCommits))
			}
			if len(payloads) < commitCount {
				if last {
					return nil, gateErr(b.name, fmt.Sprintf(
						"session reports %d commits but only %d exact completed tasks were found", commitCount, len(payloads)))
				}
				return nil, nil
			}

			extracted := aggregateMemoryCounts(payloads)
			total := memoryCountTotal(extracted)
			if total < minExtracted {
				return nil, gateErr(b.name, fmt.Sprintf(
					"session commit tasks completed with %d extracted memories, want at least %d (%v)",
					total, minExtracted, emptyMapToNil(extracted)))
			}
			claims := []CleanupClaim{{
				URI: uri, Kind: "session",
				Proof: "session get confirmed the exact committed and extracted session",
			}}
			if asBool(b.oc["cleanup_added_memories"]) {
				for _, payload := range payloads {
					added, err := commitAddedMemoryClaims(b, conf, payload)
					if err != nil {
						return nil, err
					}
					claims = append(claims, added...)
				}
			}
			out := CLIFields(statusResult)
			out["ok"], out["info"], out["extracted"] = true, info, extracted
			out["session_id"], out["uri"], out["commit_count"] = sid, uri, commitCount
			out["task_id"] = taskID
			if len(payloads) > 0 {
				out["archive_uri"] = payloads[0]["archive_uri"]
			}
			out[CleanupClaimsOutput] = claims
			return out, nil
		}

		if last {
			return nil, gateErr(b.name, fmt.Sprintf(
				"session commit task %s not complete after %d polls (status=%q)",
				taskID, retry+1, asString(task["status"])))
		}
		return nil, nil
	}

	out, attempts, err := b.poll(attempt, retry)
	if err != nil {
		return nil, err
	}
	out["attempts"] = attempts
	return out, nil
}

func completedSessionCommitPayloads(b *base, conf, sid string, last bool) ([]map[string]any, bool, error) {
	listed := runOvContext(b.context(), []string{"task", "list", "--task-type", "session_commit"}, conf, 0)
	if listed.ExitCode != 0 {
		if last {
			return nil, false, gateErr(b.name, "session commit task discovery failed: "+ExitDetail(listed))
		}
		return nil, false, nil
	}
	raw, err := resultOf(listed.Stdout)
	if err != nil {
		if last {
			return nil, false, gateErr(b.name, "session commit task list output not JSON: "+err.Error())
		}
		return nil, false, nil
	}
	tasks := exactSessionCommitTasks(asMemList(raw), sid)
	if len(tasks) == 0 {
		if last {
			return nil, false, gateErr(b.name, "no exact session_commit tasks found for session "+sid)
		}
		return nil, false, nil
	}

	payloads := make([]map[string]any, 0, len(tasks))
	for _, listedTask := range tasks {
		id := strings.TrimSpace(asString(listedTask["task_id"]))
		statusResult := runOvContext(b.context(), []string{"task", "status", id}, conf, 0)
		if statusResult.ExitCode != 0 {
			if last {
				return nil, false, gateErr(b.name, "session commit task status failed: "+ExitDetail(statusResult))
			}
			return nil, false, nil
		}
		statusRaw, err := resultOf(statusResult.Stdout)
		if err != nil {
			return nil, false, gateErr(b.name, "session commit task status output not JSON: "+err.Error())
		}
		task, ok := statusRaw.(map[string]any)
		if !ok {
			return nil, false, gateErr(b.name, fmt.Sprintf("session commit task status result is not an object: %T", statusRaw))
		}
		switch asString(task["status"]) {
		case "failed":
			return nil, false, gateErr(b.name, "session commit task failed: "+truncate(asString(task["error"]), 300))
		case "completed":
			payload, ok := task["result"].(map[string]any)
			if !ok {
				return nil, false, gateErr(b.name, fmt.Sprintf("completed session commit task result is not an object: %T", task["result"]))
			}
			payloads = append(payloads, payload)
		default:
			if last {
				return nil, false, gateErr(b.name, fmt.Sprintf("session commit task %s is not complete (status=%q)", id, asString(task["status"])))
			}
			return nil, false, nil
		}
	}
	return payloads, true, nil
}

func exactSessionCommitTasks(tasks []map[string]any, sid string) []map[string]any {
	exact := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		if asString(task["task_type"]) == "session_commit" &&
			asString(task["resource_id"]) == sid && strings.TrimSpace(asString(task["task_id"])) != "" {
			exact = append(exact, task)
		}
	}
	return exact
}

func aggregateMemoryCounts(payloads []map[string]any) map[string]any {
	counts := map[string]any{}
	total := 0
	for _, payload := range payloads {
		extracted, _ := payload["memories_extracted"].(map[string]any)
		total += memoryCountTotal(extracted)
		for key, value := range extracted {
			if key == "total" {
				continue
			}
			counts[key] = asInt(counts[key], 0) + asInt(value, 0)
		}
	}
	counts["total"] = total
	return counts
}

func latestSessionCommitTask(tasks []map[string]any, sid string) map[string]any {
	exact := exactSessionCommitTasks(tasks, sid)
	if len(exact) > 0 {
		return exact[0]
	}
	return nil
}

func sessionInfoFromGet(stdout string) (map[string]any, error) {
	rawInfo, parseErr := resultOf(stdout)
	if parseErr != nil {
		return nil, fmt.Errorf("session get output not JSON: %w", parseErr)
	}
	if rawInfo == nil {
		return map[string]any{}, nil
	}
	info, ok := rawInfo.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("session get result is not an object: %T", rawInfo)
	}
	return info, nil
}

func sessionCommittedReady(info map[string]any, minCommits, minExtracted int) (map[string]any, bool) {
	extracted, _ := info["memories_extracted"].(map[string]any)
	return extracted, asInt(info["commit_count"], 0) >= minCommits && asInt(extracted["total"], 0) >= minExtracted
}

// ── shared small helpers ────────────────────────────────────────────────────--

func lowerAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

func tokenList(v any) []string {
	if s := strings.TrimSpace(asString(v)); s != "" {
		return []string{s}
	}
	return trimNonEmpty(asStrings(v))
}

func trimNonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func filterMems(mems []map[string]any, keep func(map[string]any) bool) []map[string]any {
	out := make([]map[string]any, 0, len(mems))
	for _, m := range mems {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

func sampleMemories(mems []map[string]any, limit int) string {
	if limit <= 0 || len(mems) == 0 {
		return ""
	}
	n := limit
	if len(mems) < n {
		n = len(mems)
	}
	parts := make([]string, 0, n)
	for _, m := range mems[:n] {
		uri := truncate(asString(m["uri"]), 96)
		abstract := truncate(strings.Join(strings.Fields(asString(m["abstract"])), " "), 160)
		if abstract == "" {
			parts = append(parts, uri)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s => %s", uri, abstract))
	}
	return strings.Join(parts, " | ")
}

func asMemList(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, e := range x {
			switch m := e.(type) {
			case map[string]any:
				out = append(out, m)
			case []map[string]any, []any:
				out = append(out, asMemList(m)...)
			}
		}
		return out
	}
	return nil
}

func asMapList(v any) ([]map[string]any, bool) {
	switch x := v.(type) {
	case []map[string]any:
		return x, true
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, e := range x {
			switch m := e.(type) {
			case map[string]any:
				out = append(out, m)
			case []map[string]any, []any:
				items, ok := asMapList(m)
				if !ok {
					return nil, false
				}
				out = append(out, items...)
			default:
				return nil, false
			}
		}
		return out, true
	}
	return nil, false
}

func withSourceNode(mems []map[string]any, source string) []map[string]any {
	out := make([]map[string]any, 0, len(mems))
	for _, mem := range mems {
		copied := make(map[string]any, len(mem)+1)
		for k, v := range mem {
			copied[k] = v
		}
		if source != "" {
			copied["source_node"] = source
		}
		out = append(out, copied)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func missingTokens(s string, tokens []string) []string {
	var out []string
	for _, token := range tokens {
		if token != "" && !strings.Contains(s, token) {
			out = append(out, token)
		}
	}
	return out
}

func presentTokens(s string, tokens []string) []string {
	var out []string
	for _, token := range tokens {
		if token != "" && strings.Contains(s, token) {
			out = append(out, token)
		}
	}
	return out
}

func containsString(items []string, item string) bool {
	for _, x := range items {
		if x == item {
			return true
		}
	}
	return false
}

func resultCount(v any) int {
	switch x := v.(type) {
	case []any:
		return len(x)
	case map[string]any:
		for _, key := range []string{"items", "matches", "entries", "memories", "resources", "skills", "relations"} {
			if xs, ok := x[key].([]any); ok {
				return len(xs)
			}
		}
	}
	return 0
}

func allTruthy(v any) bool {
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			if !boolish(x) {
				return false
			}
		}
		return true
	}
	return boolish(v)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func emptyMapToNil(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func memoryCountTotal(counts map[string]any) int {
	if total := asInt(counts["total"], -1); total >= 0 {
		return total
	}
	total := 0
	for _, value := range counts {
		total += asInt(value, 0)
	}
	return total
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func jsonStr(v any) string {
	return fmt.Sprintf("%v", v)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
