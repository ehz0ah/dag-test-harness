package evidence

import "testing"

func TestHasBusinessErrorRecursesAndFailsClosedOnKnownShapes(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{"success", map[string]any{"ok": true, "content": []any{"stored"}}, false},
		{"camel error", map[string]any{"isError": true}, true},
		{"nested false ok", map[string]any{"result": map[string]any{"ok": false}}, true},
		{"false success", map[string]any{"success": false}, true},
		{"error status", map[string]any{"status": "ERROR"}, true},
		{"nested text error", []any{map[string]any{"content": "Error: unauthorized"}}, true},
		{"json text envelope error", map[string]any{"type": "text", "text": `{"status":"error","message":"unauthorized"}`}, true},
		{"json text envelope false success", map[string]any{"type": "text", "text": `{"success":false}`}, true},
		{"json text envelope success", map[string]any{"type": "text", "text": `{"status":"ok","success":true}`}, false},
		{"ordinary error word", "the error budget is healthy", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := HasBusinessError(test.value); got != test.want {
				t.Fatalf("HasBusinessError(%v) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestToolSetNormalizesNames(t *testing.T) {
	set := ToolSet{}
	set.Add("  OpenViking_Search ")
	if !set.Contains("openviking_search") || set.Contains("openviking_find") {
		t.Fatalf("unexpected normalized tool set: %v", set)
	}
}

func TestMemoryMapsAcceptsOnlyStructuredItems(t *testing.T) {
	items := memoryMaps([]any{map[string]any{"uri": "viking://user/a"}, "viking://user/prompt-only"})
	if len(items) != 1 || items[0]["uri"] != "viking://user/a" {
		t.Fatalf("memoryMaps = %v", items)
	}
}
