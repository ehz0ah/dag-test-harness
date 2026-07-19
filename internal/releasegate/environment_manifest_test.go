package releasegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentManifestIsImmutableAndReplayIsBoundByHash(t *testing.T) {
	root := t.TempDir()
	lockDir := filepath.Join(root, "locks", "codex")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "package-lock.json"), []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := Manifest{SchemaVersion: ManifestSchemaVersion, RunID: "old", Mode: SourceLatest, CreatedAt: time.Now(), Sources: []Source{{
		Repository: Repository{Name: "OpenViking", URL: "https://github.com/volcengine/OpenViking.git", Ref: "refs/heads/main"}, Commit: strings.Repeat("a", 40), FetchedAt: time.Now(),
	}}}
	harnesses := HarnessManifest{SchemaVersion: HarnessManifestSchemaVersion, CreatedAt: time.Now(), Artifacts: []HarnessArtifact{{
		Harness: "codex", Ecosystem: "npm", Package: "@openai/codex", Version: "1.0.0", Lockfile: "locks/codex/package-lock.json",
		LockfileSHA256: "a", InstalledSHA256: "b", ExecutableSHA256: "c",
		RegistryIdentities: []string{"registry.npmjs.org"}, InstallCommand: []string{"npm", "ci"},
	}}}
	manifest := EnvironmentManifest{EnvironmentID: "run-1", Mode: SourceLatest, Source: source, Harnesses: harnesses,
		Platform: NewPlatformIdentity(map[string]string{"go": "go1"}), Gate: GateIdentity{Cases: []string{"case"}, Definition: "gate", OvtestBinary: "binary"},
		Trust:         TrustDecision{CredentialedExecution: true, Authority: "operator-flag"},
		Configuration: map[string]string{"qualification_attempts": "1"},
	}
	path := filepath.Join(root, "environment-manifest.json")
	if err := WriteEnvironmentManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvironmentManifest(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvironmentManifest(path, manifest); err == nil {
		t.Fatal("environment manifest was overwritten")
	}

	replay := manifest
	replay.EnvironmentID = "run-2"
	replay.Mode = SourceManifest
	replay.ReplayOf = path
	replayPath := filepath.Join(root, "replay.json")
	if err := WriteEnvironmentManifest(replayPath, replay); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(mustReadBytes(t, path), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvironmentManifest(replayPath); err == nil {
		t.Fatal("changed replay manifest accepted")
	}
}

func TestValidateEnvironmentReplayRejectsGateAndToolchainDrift(t *testing.T) {
	base := EnvironmentManifest{
		Source:        Manifest{Sources: []Source{{Repository: Repository{Name: "OpenViking", URL: "https://example.test/ov.git", Ref: "refs/heads/main"}, Commit: strings.Repeat("a", 40)}}},
		Harnesses:     HarnessManifest{Artifacts: []HarnessArtifact{{Harness: "codex", Version: "1", InstalledSHA256: "tree", RegistryIdentities: []string{"registry.npmjs.org"}, InstallCommand: []string{"npm", "ci"}}}},
		Platform:      PlatformIdentity{OS: "darwin", Arch: "arm64", Toolchain: map[string]string{"go": "1"}},
		Gate:          GateIdentity{Cases: []string{"case"}, Definition: "gate", OvtestBinary: "binary"},
		Plugins:       map[string]PluginProvenance{"codex": {Path: "plugin", TreeSHA256: "plugin"}},
		Configuration: map[string]string{"attempts": "1"},
	}
	current := base
	if err := ValidateEnvironmentReplay(current, base); err != nil {
		t.Fatal(err)
	}
	current.Gate.Definition = "changed"
	if err := ValidateEnvironmentReplay(current, base); err == nil {
		t.Fatal("gate drift accepted")
	}
	current = base
	current.Platform.Toolchain = map[string]string{"go": "2"}
	if err := ValidateEnvironmentReplay(current, base); err == nil {
		t.Fatal("toolchain drift accepted")
	}
}

func TestValidateEnvironmentReplayRejectsBaselineDrift(t *testing.T) {
	base := EnvironmentManifest{
		Source:        Manifest{Sources: []Source{{Repository: Repository{Name: "OpenViking", URL: "https://example.test/ov.git", Ref: "refs/heads/feature"}, Commit: strings.Repeat("a", 40)}}},
		Harnesses:     HarnessManifest{Artifacts: []HarnessArtifact{}},
		Platform:      PlatformIdentity{OS: "darwin", Arch: "arm64", Toolchain: map[string]string{"go": "1"}},
		Gate:          GateIdentity{Cases: []string{"case"}, Definition: "gate", OvtestBinary: "binary"},
		Configuration: map[string]string{"attempts": "1"},
	}
	baseline := base.Source
	baseline.Sources = append([]Source(nil), base.Source.Sources...)
	baseline.Sources[0].Repository.Ref = "refs/heads/main"
	baseline.Sources[0].Commit = strings.Repeat("b", 40)
	base.Baseline = &baseline
	current := base
	current.Baseline = nil
	if err := ValidateEnvironmentReplay(current, base); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("baseline drift error = %v", err)
	}
}

func mustReadBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
