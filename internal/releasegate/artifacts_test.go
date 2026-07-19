package releasegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestRunArtifactsRetainFailureRuntimeButRemoveSecrets(t *testing.T) {
	a, err := NewRunArtifacts(t.TempDir(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.RuntimeDir, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.SecretsDir, "credential"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.EvidenceDir, "log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Finalize(false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.RuntimeDir); err != nil {
		t.Fatalf("failure runtime removed: %v", err)
	}
	if _, err := os.Stat(a.SecretsDir); !os.IsNotExist(err) {
		t.Fatalf("secret material remains: %v", err)
	}
	if _, err := os.Stat(a.EvidenceDir); err != nil {
		t.Fatalf("failure evidence removed: %v", err)
	}
}

func TestSuccessfulArtifactsDropFullEvidenceAndRuntimeByDefault(t *testing.T) {
	a, err := NewRunArtifacts(t.TempDir(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Finalize(true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.EvidenceDir); !os.IsNotExist(err) {
		t.Fatalf("successful full evidence remains: %v", err)
	}
	if _, err := os.Stat(a.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("successful runtime remains: %v", err)
	}
}

func TestSuccessfulArtifactsCanRetainRedactedEvidence(t *testing.T) {
	a, err := NewRunArtifacts(t.TempDir(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Finalize(true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.EvidenceDir); err != nil {
		t.Fatalf("explicitly retained evidence removed: %v", err)
	}
}

func TestAttemptArtifactsAreFreshAndNestedUnderRun(t *testing.T) {
	run, err := NewRunArtifacts(t.TempDir(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAttemptArtifacts(run, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewAttemptArtifacts(run, "candidate-2")
	if err != nil {
		t.Fatal(err)
	}
	if a.RuntimeDir == b.RuntimeDir || a.SecretsDir == b.SecretsDir || a.EvidenceDir == b.EvidenceDir {
		t.Fatalf("attempts share state: %+v %+v", a, b)
	}
	for _, path := range []string{a.RuntimeDir, a.SecretsDir, a.EvidenceDir, b.RuntimeDir, b.SecretsDir, b.EvidenceDir} {
		if !pathWithin(run.RunDir, path) {
			t.Fatalf("attempt path escaped run: %s", path)
		}
	}
}

func TestPruneExpiredRunsDeletesProtectedRefs(t *testing.T) {
	origin := createOrigin(t)
	root := t.TempDir()
	p, err := PrepareSources(context.Background(), SourceConfig{Root: root, RunID: "old-run", Mode: SourceLatest,
		Repositories: []Repository{{Name: "demo", URL: origin, Ref: "refs/heads/main"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "runs", "old-run"), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneExpiredRuns(context.Background(), root, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old-run" {
		t.Fatalf("removed = %v", removed)
	}
	cmd := exec.Command("git", "--git-dir", filepath.Join(root, "mirrors", "demo.git"), "show-ref", "--verify", "refs/ovtest/runs/old-run/demo")
	if err := cmd.Run(); err == nil {
		t.Fatal("protected ref remains after manifest retention expired")
	}
}

func TestPruneExpiredIncompleteRunDeletesRefsAndDirectory(t *testing.T) {
	origin := createOrigin(t)
	root := t.TempDir()
	p, err := PrepareSources(context.Background(), SourceConfig{Root: root, RunID: "seed-run", Mode: SourceLatest,
		Repositories: []Repository{{Name: "demo", URL: origin, Ref: "refs/heads/main"}}})
	if err != nil {
		t.Fatal(err)
	}
	sha := p.Manifest.Sources[0].Commit
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	incomplete := filepath.Join(root, "runs", "incomplete-run")
	if err := os.MkdirAll(incomplete, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Join(root, "mirrors", "demo.git"), "update-ref", "refs/ovtest/runs/incomplete-run/demo", sha)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(incomplete, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneExpiredRuns(context.Background(), root, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(removed, "incomplete-run") {
		t.Fatalf("removed = %v", removed)
	}
	if _, err := os.Stat(incomplete); !os.IsNotExist(err) {
		t.Fatalf("incomplete run remains: %v", err)
	}
	cmd := exec.Command("git", "--git-dir", filepath.Join(root, "mirrors", "demo.git"), "show-ref", "--verify", "refs/ovtest/runs/incomplete-run/demo")
	if err := cmd.Run(); err == nil {
		t.Fatal("incomplete protected ref remains")
	}
}
