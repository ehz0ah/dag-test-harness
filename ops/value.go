package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// JSON helpers tolerate ov's leading `cmd: ...` echo.

func parseJSON(stdout string) (any, error) {
	if i := strings.IndexByte(stdout, '{'); i >= 0 {
		stdout = stdout[i:]
	}
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// resultOf returns the "result" field of an ov JSON response. A valid-but-non-
// object top level mirrors Python's `(parsed or {}).get`: a truthy non-dict is
// a schema error; a falsy one yields no result and no error.
func resultOf(stdout string) (any, error) {
	v, err := parseJSON(stdout)
	if err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		return m["result"], nil
	}
	if jsonTruthy(v) {
		return nil, fmt.Errorf("top-level JSON is not an object")
	}
	return nil, nil
}

func jsonTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// resultMap returns the "result" object as a map (empty if absent/not an object).
func resultMap(stdout string) (map[string]any, error) {
	r, err := resultOf(stdout)
	if err != nil {
		return nil, err
	}
	if m, ok := r.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

// memoriesOf parses search/find context lists into [{uri,score,abstract}] plus
// a parse-error string so parse failures land in the evidence trace.
func memoriesOf(stdout string) ([]map[string]any, string) {
	m, err := resultMap(stdout)
	if err != nil {
		return nil, "memories JSON parse failed: " + err.Error()
	}
	out := []map[string]any{}
	found := false
	for _, key := range []string{"memories", "resources", "skills"} {
		rawValue, ok := m[key]
		if !ok {
			continue
		}
		found = true
		raw, ok := rawValue.([]any)
		if !ok {
			return nil, fmt.Sprintf("memories JSON parse failed: result.%s is not an array", key)
		}
		for i, item := range raw {
			mm, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Sprintf("memories JSON parse failed: result.%s[%d] is not an object", key, i)
			}
			out = append(out, map[string]any{
				"uri": mm["uri"], "score": mm["score"], "abstract": mm["abstract"], "context_type": mm["context_type"]})
		}
	}
	if !found {
		return nil, "memories JSON parse failed: result.memories/resources/skills missing"
	}
	return out, ""
}

// Small coercion helpers: CLI and JSON data is intentionally untyped at op edges.

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n
		}
	}
	return def
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func asStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, asString(e))
		}
		return out
	}
	return nil
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
