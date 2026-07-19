package ops

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureFileWritesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.md")

	out, err := newOp(FixtureFile, "fixture_file", map[string]any{
		"path":    path,
		"content": "resource marker F-123",
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("fixture file should pass: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "resource marker F-123" || out["path"] != path {
		t.Fatalf("file=%q out=%v", got, out)
	}
}

func TestFixtureServerServesContent(t *testing.T) {
	out, err := newOp(FixtureServer, "fixture_server", map[string]any{
		"path":    "/remote-resource.md",
		"content": "remote marker R-123",
	}).Process(map[string]any{})
	if err != nil {
		t.Fatalf("fixture server should pass: %v", err)
	}
	resp, err := http.Get(out["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "remote marker R-123" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	CloseFixtureServers()
}

func TestCloseFixtureServersStopsServers(t *testing.T) {
	out, err := newOp(FixtureServer, "fixture_server", map[string]any{
		"path":    "/remote-resource.md",
		"content": "hello fixture",
	}).Process(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	url := out["url"].(string)
	CloseFixtureServers()
	_, err = http.Get(url)
	if err == nil {
		t.Fatalf("fixture server still accepted requests after CloseFixtureServers")
	}
}
