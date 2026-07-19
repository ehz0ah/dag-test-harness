package releasegate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteDefaultLocalOpenVikingTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "template.json")
	if err := WriteDefaultLocalOpenVikingTemplate(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	server := config["server"].(map[string]any)
	if server["auth_mode"] != "api_key" {
		t.Fatalf("auth mode = %v", server["auth_mode"])
	}
	embedding := config["embedding"].(map[string]any)
	if embedding["max_concurrent"] != float64(2) {
		t.Fatalf("embedding max_concurrent = %v, want 2", embedding["max_concurrent"])
	}
	if string(raw) == "" || !containsAll(string(raw), "$OPENVIKING_LLM_API_KEY", "$OPENVIKING_EMBEDDING_API_KEY") {
		t.Fatal("template must contain provider environment references")
	}
}

func containsAll(text string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}
