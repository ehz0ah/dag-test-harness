package releasegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHashTreeIsDeterministicAndContentSensitive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := HashTree(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashTree(root)
	if err != nil || first != second {
		t.Fatalf("hashes = %q %q err=%v", first, second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, _ := HashTree(root)
	if changed == first {
		t.Fatal("tree hash ignored content change")
	}
}

func TestHashTreeTracksOnlySemanticModeAndSymlinkMetadata(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "tool")
	if err := os.WriteFile(file, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := HashTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	nonExecutable, _ := HashTree(root)
	if nonExecutable != base {
		t.Fatal("non-executable permission metadata changed canonical tree hash")
	}
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, _ := HashTree(root)
	if executable == base {
		t.Fatal("executable bit was ignored")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("tool", link); err != nil {
		t.Fatal(err)
	}
	linked, _ := HashTree(root)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", link); err != nil {
		t.Fatal(err)
	}
	relinked, _ := HashTree(root)
	if linked == relinked {
		t.Fatal("symlink target was ignored")
	}
}

func TestExecutableProvenanceRecordsAbsolutePathAndVersion(t *testing.T) {
	p, err := InspectExecutable(context.Background(), "git", "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Path) || p.Version == "" {
		t.Fatalf("provenance = %+v", p)
	}
}

func TestInspectExecutableArtifactDoesNotInvokeExecutable(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "service")
	if err := os.WriteFile(binary, []byte("not an executable script"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := InspectExecutableArtifact(binary, "health-version")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != binary || got.Version != "health-version" || got.SHA256 == "" {
		t.Fatalf("provenance = %+v", got)
	}
}

func TestRunManifestIsImmutableAndReferencesSourceHash(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.json")
	if err := WriteManifest(sourcePath, Manifest{SchemaVersion: ManifestSchemaVersion, RunID: "r", Mode: SourceLatest,
		Sources: []Source{{Repository: Repository{Name: "demo", URL: "https://example.com/demo.git", Ref: "refs/heads/main"}, Commit: strings.Repeat("a", 40), FetchedAt: time.Now().UTC()}}}); err != nil {
		t.Fatal(err)
	}
	environmentPath := filepath.Join(dir, "environment.json")
	if err := os.WriteFile(environmentPath, []byte("environment"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := RunManifest{SchemaVersion: RunManifestSchemaVersion, RunID: "r", Attempt: "candidate-1", Suite: "smoke", Cases: []string{"openviking-service-baseline"}, SourceManifestPath: sourcePath, EnvironmentManifestPath: environmentPath}
	path := filepath.Join(dir, "run.json")
	if err := WriteRunManifest(path, run); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadRunManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := FileSHA256(sourcePath)
	if loaded.SourceManifestSHA256 != want {
		t.Fatalf("source hash = %q want %q", loaded.SourceManifestSHA256, want)
	}
	if err := WriteRunManifest(path, run); err == nil {
		t.Fatal("run manifest was overwritten")
	}
}
