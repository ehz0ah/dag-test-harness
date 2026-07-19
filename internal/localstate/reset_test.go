package localstate

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResetDeletesRuntimeStateButKeepsConfig(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".ovtest")
	confDir := filepath.Join(stateDir, "openviking")
	workspace := filepath.Join(confDir, "workspace")
	template := filepath.Join(stateDir, "hermes", "template")

	writeFile(t, filepath.Join(confDir, "ov.conf"), `{"storage":{"workspace":`+quote(workspace)+`},"server":{"host":"127.0.0.1","port":1933}}`)
	writeFile(t, filepath.Join(confDir, "ovcli.conf"), `{"url":"http://127.0.0.1:1933","api_key":"test"}`)
	writeFile(t, filepath.Join(template, "config.yaml"), "model: test\n")
	writeFile(t, filepath.Join(template, "cache", "stale.bin"), "stale")
	writeFile(t, filepath.Join(stateDir, "hermes", "capture-1234", "sessions", "state.db"), "db")
	writeFile(t, filepath.Join(stateDir, "fixtures", "remote.md"), "fixture")
	writeFile(t, filepath.Join(workspace, "viking", "default", "memory.db"), "db")

	t.Setenv("OV_TEST_STATE_DIR", stateDir)
	t.Setenv("OV_TEST_CONF_DIR", confDir)
	restore := stubProbe(func(string) (bool, error) { return false, nil })
	defer restore()

	removed, err := Reset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(removed) == 0 {
		t.Fatalf("Reset reported no removed paths")
	}

	assertExists(t, filepath.Join(confDir, "ov.conf"))
	assertExists(t, filepath.Join(confDir, "ovcli.conf"))
	assertExists(t, filepath.Join(template, "config.yaml"))
	assertMissing(t, filepath.Join(template, "cache"))
	assertMissing(t, filepath.Join(stateDir, "hermes", "capture-1234"))
	assertMissing(t, filepath.Join(stateDir, "fixtures"))
	assertMissing(t, workspace)
}

func TestResetRefusesWhenConfiguredOpenVikingIsReachable(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".ovtest")
	confDir := filepath.Join(stateDir, "openviking")
	workspace := filepath.Join(confDir, "workspace")
	writeFile(t, filepath.Join(confDir, "ov.conf"), `{"storage":{"workspace":`+quote(workspace)+`},"server":{"host":"127.0.0.1","port":1933}}`)
	writeFile(t, filepath.Join(workspace, "viking", "default", "memory.db"), "db")

	t.Setenv("OV_TEST_STATE_DIR", stateDir)
	t.Setenv("OV_TEST_CONF_DIR", confDir)
	restore := stubProbe(func(url string) (bool, error) {
		if url != "http://127.0.0.1:1933" {
			t.Fatalf("probe URL = %q", url)
		}
		return true, nil
	})
	defer restore()

	_, err := Reset()
	if err == nil || !strings.Contains(err.Error(), "reachable") {
		t.Fatalf("Reset error = %v, want reachable refusal", err)
	}
	assertExists(t, filepath.Join(workspace, "viking", "default", "memory.db"))
}

func TestResetProbesOpenVikingConfigNotCLIConfig(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".ovtest")
	confDir := filepath.Join(stateDir, "openviking")
	workspace := filepath.Join(confDir, "workspace")
	writeFile(t, filepath.Join(confDir, "ov.conf"), `{"storage":{"workspace":`+quote(workspace)+`},"server":{"host":"127.0.0.1","port":19444}}`)
	writeFile(t, filepath.Join(workspace, "viking", "default", "memory.db"), "db")

	t.Setenv("OV_TEST_STATE_DIR", stateDir)
	t.Setenv("OV_TEST_CONF_DIR", confDir)
	restore := stubProbe(func(url string) (bool, error) {
		if url != "http://127.0.0.1:19444" {
			t.Fatalf("probe URL = %q", url)
		}
		return true, nil
	})
	defer restore()

	_, err := Reset()
	if err == nil || !strings.Contains(err.Error(), "reachable") {
		t.Fatalf("Reset error = %v, want reachable refusal from ov.conf", err)
	}
	assertExists(t, filepath.Join(workspace, "viking", "default", "memory.db"))
}

func TestResetRefusesWorkspaceOutsideStateDir(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".ovtest")
	confDir := filepath.Join(stateDir, "openviking")
	outside := filepath.Join(root, "outside-workspace")
	writeFile(t, filepath.Join(confDir, "ov.conf"), `{"storage":{"workspace":`+quote(outside)+`}}`)

	t.Setenv("OV_TEST_STATE_DIR", stateDir)
	t.Setenv("OV_TEST_CONF_DIR", confDir)
	restore := stubProbe(func(string) (bool, error) { return false, nil })
	defer restore()

	_, err := Reset()
	if err == nil || !strings.Contains(err.Error(), "outside state dir") {
		t.Fatalf("Reset error = %v, want outside-state-dir refusal", err)
	}
}

func TestResetRefusesWorkspaceEscapingThroughSymlink(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".ovtest")
	confDir := filepath.Join(stateDir, "openviking")
	outside := filepath.Join(root, "outside")
	workspace := filepath.Join(stateDir, "link", "workspace")
	writeFile(t, filepath.Join(outside, "workspace", "memory.db"), "db")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stateDir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writeFile(t, filepath.Join(confDir, "ov.conf"), `{"storage":{"workspace":`+quote(workspace)+`}}`)

	t.Setenv("OV_TEST_STATE_DIR", stateDir)
	t.Setenv("OV_TEST_CONF_DIR", confDir)
	restore := stubProbe(func(string) (bool, error) { return false, nil })
	defer restore()

	_, err := Reset()
	if err == nil || !strings.Contains(err.Error(), "outside state dir") {
		t.Fatalf("Reset error = %v, want symlink escape refusal", err)
	}
	assertExists(t, filepath.Join(outside, "workspace", "memory.db"))
}

func TestProbeOpenVikingFailsClosedOnAmbiguousLocalProbe(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	orig := probeTimeout
	probeTimeout = 50 * time.Millisecond
	defer func() { probeTimeout = orig }()

	reachable, err := probeOpenViking(srv.URL)
	if err == nil {
		t.Fatalf("probeOpenViking reachable=%v err=nil, want ambiguous local probe error", reachable)
	}
	if reachable {
		t.Fatalf("probeOpenViking reachable=true on timed-out health probe")
	}
}

func stubProbe(fn func(string) (bool, error)) func() {
	orig := probeOpenViking
	probeOpenViking = fn
	return func() { probeOpenViking = orig }
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s should exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should be missing, stat err=%v", path, err)
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
