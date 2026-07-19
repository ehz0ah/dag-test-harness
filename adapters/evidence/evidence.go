// Package evidence contains transport-independent evidence checks shared by
// agent adapters. Event decoding stays in each adapter because host schemas
// intentionally differ.
package evidence

import (
	"encoding/json"
	"fmt"
	"strings"

	"code.byted.org/data-arch/ovtest/dag"
	root "code.byted.org/data-arch/ovtest/ops"
)

// MemoryURI extracts one concrete URI from a structurally verified Find result.
// It is used to ensure destructive tool prompts receive the exact object proven
// to exist, rather than asking the model to search and delete speculatively.
var MemoryURI = root.NewFactory(dag.Meta{
	Inputs: []string{"memories", "after"}, Outputs: []string{"uri"},
}, false, func(ctx *root.OpContext) root.ExecFunc {
	return func(in map[string]any) (map[string]any, error) {
		prefix := strings.ToLower(root.AsString(ctx.Config()["uri_prefix"]))
		for _, item := range memoryMaps(in["memories"]) {
			uri := root.AsString(item["uri"])
			if uri != "" && (prefix == "" || strings.HasPrefix(strings.ToLower(uri), prefix)) {
				return map[string]any{"uri": uri}, nil
			}
		}
		return nil, ctx.GateErr(fmt.Sprintf("no concrete memory URI matched prefix %q", prefix))
	}
})

func memoryMaps(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		var out []map[string]any
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// HasBusinessError rejects successful transport envelopes that contain a
// semantic error returned as ordinary result content.
func HasBusinessError(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if failed, ok := x["isError"].(bool); ok && failed {
			return true
		}
		if failed, ok := x["is_error"].(bool); ok && failed {
			return true
		}
		if okValue, ok := x["ok"].(bool); ok && !okValue {
			return true
		}
		if success, ok := x["success"].(bool); ok && !success {
			return true
		}
		if status, ok := x["status"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "error", "failed", "failure":
				return true
			}
		}
		if errValue, ok := x["error"]; ok && errValue != nil && strings.TrimSpace(fmt.Sprint(errValue)) != "" {
			return true
		}
		for _, child := range x {
			if HasBusinessError(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if HasBusinessError(child) {
				return true
			}
		}
	case string:
		trimmed := strings.TrimSpace(x)
		value := strings.ToLower(trimmed)
		if strings.HasPrefix(value, "error:") || strings.HasPrefix(value, "failed:") || strings.HasPrefix(value, "failure:") {
			return true
		}
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var decoded any
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				return HasBusinessError(decoded)
			}
		}
	}
	return false
}

// ToolSet is the normalized set used after adapter-specific tool-name decoding.
type ToolSet map[string]bool

func (s ToolSet) Add(name string) { s[strings.ToLower(strings.TrimSpace(name))] = true }

func (s ToolSet) Contains(name string) bool { return s[strings.ToLower(strings.TrimSpace(name))] }
