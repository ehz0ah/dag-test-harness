package support

import "testing"

func TestResourceRootUsesRunOwnedSafePrefix(t *testing.T) {
	t.Setenv("OV_TEST_RUN_ID", "Run 42/Canary")
	if got, want := ResourceRoot("Claude Code", "ignored"), "viking://resources/ovtest-runs/run-42-canary/claude-code"; got != want {
		t.Fatalf("ResourceRoot = %q, want %q", got, want)
	}
	t.Setenv("OV_TEST_RUN_ID", "")
	if got, want := ResourceRoot("Codex", "ABC-123"), "viking://resources/ovtest-runs/abc-123/codex"; got != want {
		t.Fatalf("fallback ResourceRoot = %q, want %q", got, want)
	}
}
