package runner

import "strconv"

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolish(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	}
	return true
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case jsonNumber:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil
	}
	return 0, false
}

type jsonNumber string
