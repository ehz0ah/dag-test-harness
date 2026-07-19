package ops

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretStateDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OV_TEST_SECRET_STATE_DIR", root)

	first := SecretStateDir("OpenCode/config", "/tmp/run-a/state")
	second := SecretStateDir("OpenCode/config", "/tmp/run-a/state")
	if first != second {
		t.Fatalf("SecretStateDir is not stable: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, filepath.Join(root, "opencode-config")+string(filepath.Separator)) {
		t.Fatalf("SecretStateDir %q escapes/surprises root %q", first, root)
	}
	if first == SecretStateDir("OpenCode/config", "/tmp/run-b/state") {
		t.Fatal("different state directories must not collide")
	}
}

func TestSecretStateDirDisabled(t *testing.T) {
	t.Setenv("OV_TEST_SECRET_STATE_DIR", "")
	if got := SecretStateDir("codex", "/tmp/state"); got != "" {
		t.Fatalf("SecretStateDir = %q, want empty", got)
	}
}

func TestQueueScopeKeyFilePrefersEnvironmentScope(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "scope.key")
	t.Setenv("OPENVIKING_QUEUE_SCOPE_KEY_FILE", explicit)
	if got := QueueScopeKeyFile("codex", "/tmp/state"); got != explicit {
		t.Fatalf("QueueScopeKeyFile = %q, want %q", got, explicit)
	}
}
