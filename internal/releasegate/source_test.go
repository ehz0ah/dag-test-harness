package releasegate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareLatestAndReplayManifest(t *testing.T) {
	origin := createOrigin(t)
	root := t.TempDir()
	cfg := SourceConfig{
		Root:         root,
		RunID:        "run-1",
		Mode:         SourceLatest,
		Repositories: []Repository{{Name: "demo", URL: origin, Ref: "refs/heads/main"}},
	}
	prepared, err := PrepareSources(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	if got := mustRead(t, filepath.Join(prepared.Worktrees["demo"], "value.txt")); got != "one\n" {
		t.Fatalf("worktree content = %q", got)
	}
	if prepared.Manifest.Mode != SourceLatest || len(prepared.Manifest.Sources) != 1 {
		t.Fatalf("manifest = %+v", prepared.Manifest)
	}
	sha := prepared.Manifest.Sources[0].Commit
	if out := git(t, filepath.Join(root, "mirrors", "demo.git"), "rev-parse", "refs/ovtest/runs/run-1/demo"); trim(out) != sha {
		t.Fatalf("protected ref = %q, want %q", trim(out), sha)
	}

	advanceOrigin(t, origin)
	latestAgain, err := PrepareSources(context.Background(), SourceConfig{
		Root: root, RunID: "run-latest-2", Mode: SourceLatest,
		Repositories: []Repository{{Name: "demo", URL: origin, Ref: "refs/heads/main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = latestAgain.Close(context.Background()) })
	if out := git(t, filepath.Join(root, "mirrors", "demo.git"), "rev-parse", "refs/ovtest/runs/run-1/demo"); trim(out) != sha {
		t.Fatalf("latest refresh pruned protected ref: %q, want %q", trim(out), sha)
	}
	replayCfg := SourceConfig{Root: root, RunID: "run-2", Mode: SourceManifest, ManifestPath: prepared.ManifestPath}
	replayed, err := PrepareSources(context.Background(), replayCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replayed.Close(context.Background()) })
	if got := mustRead(t, filepath.Join(replayed.Worktrees["demo"], "value.txt")); got != "one\n" {
		t.Fatalf("manifest replay used latest content: %q", got)
	}
	if replayed.Manifest.Sources[0].Commit != sha {
		t.Fatalf("replay commit = %s want %s", replayed.Manifest.Sources[0].Commit, sha)
	}
}

func TestSourceConfigRejectsUnsupportedModeAndUnsafeNames(t *testing.T) {
	for _, cfg := range []SourceConfig{
		{Root: t.TempDir(), RunID: "r", Mode: "workspace"},
		{Root: t.TempDir(), RunID: "r", Mode: SourceLatest, Repositories: []Repository{{Name: "../escape", URL: "x", Ref: "main"}}},
		{Root: t.TempDir(), RunID: "../escape", Mode: SourceLatest},
	} {
		if _, err := PrepareSources(context.Background(), cfg); err == nil {
			t.Fatalf("PrepareSources(%+v) succeeded", cfg)
		}
	}
}

func TestDefaultSourceIsOnlyOpenVikingAndCandidateRefsAreConstrained(t *testing.T) {
	repos := DefaultRepositories()
	if len(repos) != 1 || repos[0].Name != "OpenViking" || repos[0].Ref != "refs/heads/main" {
		t.Fatalf("default repositories = %+v", repos)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/release/v1", "refs/pull/3232/head", "refs/pull/3232/merge", strings.Repeat("a", 40)} {
		if _, err := OpenVikingRepository(ref); err != nil {
			t.Fatalf("OpenVikingRepository(%q): %v", ref, err)
		}
	}
	for _, ref := range []string{"main", "refs/tags/v1", "refs/pull/abc/head", "refs/pull/1/other", "refs/heads/../escape", strings.Repeat("z", 40)} {
		if _, err := OpenVikingRepository(ref); err == nil {
			t.Fatalf("unsafe candidate ref %q accepted", ref)
		}
	}
}

func TestManifestWriteIsImmutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m := Manifest{SchemaVersion: ManifestSchemaVersion, RunID: "r", Mode: SourceLatest,
		Sources: []Source{{Repository: Repository{Name: "demo", URL: "https://example.com/demo.git", Ref: "refs/heads/main"}, Commit: strings.Repeat("a", 40), FetchedAt: time.Now().UTC()}}}
	if err := WriteManifest(path, m); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(path, m); err == nil {
		t.Fatal("second manifest write must fail")
	}
}

func TestManifestRejectsUnsafeCommitRefAndCredentialURL(t *testing.T) {
	base := Source{Repository: Repository{Name: "demo", URL: "https://example.com/demo.git", Ref: "refs/heads/main"}, Commit: strings.Repeat("a", 40), FetchedAt: time.Now().UTC()}
	for _, mutate := range []func(*Source){
		func(s *Source) { s.Commit = "--help" },
		func(s *Source) { s.Ref = "--upload-pack=evil" },
		func(s *Source) { s.Ref = "refs/heads/../evil" },
		func(s *Source) { s.URL = "https://token:secret@example.com/demo.git" },
	} {
		source := base
		mutate(&source)
		path := filepath.Join(t.TempDir(), "manifest.json")
		raw, err := json.Marshal(Manifest{SchemaVersion: ManifestSchemaVersion, RunID: "r", Mode: SourceLatest, Sources: []Source{source}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadManifest(path); err == nil {
			t.Fatalf("unsafe source accepted: %+v", source)
		}
	}
}

func TestConcurrentPrepareSerializesSharedMirror(t *testing.T) {
	origin := createOrigin(t)
	root := t.TempDir()
	ctx := context.Background()
	errs := make(chan error, 2)
	for _, runID := range []string{"run-a", "run-b"} {
		go func(runID string) {
			p, err := PrepareSources(ctx, SourceConfig{Root: root, RunID: runID, Mode: SourceLatest,
				Repositories: []Repository{{Name: "demo", URL: origin, Ref: "refs/heads/main"}}})
			if err == nil {
				err = p.Close(ctx)
			}
			errs <- err
		}(runID)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareCancellationLeavesNoWorktree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	_, err := PrepareSources(ctx, SourceConfig{Root: root, RunID: "r", Mode: SourceLatest,
		Repositories: []Repository{{Name: "demo", URL: createOrigin(t), Ref: "refs/heads/main"}}})
	if err == nil {
		t.Fatal("cancelled prepare succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(root, "runs", "r", "worktrees", "demo")); !os.IsNotExist(statErr) {
		t.Fatalf("worktree remains after cancellation: %v", statErr)
	}
}

func createOrigin(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	git(t, work, "init", "-b", "main")
	git(t, work, "config", "user.name", "Test")
	git(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "value.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "value.txt")
	git(t, work, "commit", "-m", "one")
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, "", "clone", "--bare", work, origin)
	return origin
}

func advanceOrigin(t *testing.T, origin string) {
	t.Helper()
	work := t.TempDir()
	git(t, "", "clone", origin, work)
	git(t, work, "config", "user.name", "Test")
	git(t, work, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(work, "value.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "commit", "-am", "two")
	git(t, work, "push", "origin", "HEAD:main")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
