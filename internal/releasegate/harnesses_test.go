package releasegate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareNPMHarnessLatestAndExactReplay(t *testing.T) {
	firstRoot := t.TempDir()
	var progress []string
	first, err := PrepareHarnesses(context.Background(), HarnessInstallConfig{
		Mode: SourceLatest, RunRoot: firstRoot, CacheRoot: filepath.Join(firstRoot, "cache"), EvidenceDir: filepath.Join(firstRoot, "evidence"),
		Harnesses: []string{"codex"}, ResolveNPM: func(context.Context, string) (string, error) { return "1.2.3", nil }, RunCommand: fakeNPMInstall,
		Progress: func(harness, phase string) { progress = append(progress, harness+":"+phase) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Manifest.Artifacts) != 1 || first.Manifest.Artifacts[0].Version != "1.2.3" {
		t.Fatalf("manifest = %+v", first.Manifest)
	}
	if _, err := os.Stat(first.Binaries["codex"]); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(progress, ","), "codex:acquiring,codex:verified"; got != want {
		t.Fatalf("progress = %q, want %q", got, want)
	}

	secondRoot := t.TempDir()
	second, err := PrepareHarnesses(context.Background(), HarnessInstallConfig{
		Mode: SourceManifest, RunRoot: secondRoot, CacheRoot: filepath.Join(secondRoot, "cache"), EvidenceDir: filepath.Join(secondRoot, "evidence"),
		Harnesses: []string{"codex"}, Replay: &first.Manifest, ReplayRoot: firstRoot,
		ResolveNPM: func(context.Context, string) (string, error) { return "", fmt.Errorf("replay resolved latest") }, RunCommand: fakeNPMInstall,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.Artifacts[0].InstalledSHA256 != first.Manifest.Artifacts[0].InstalledSHA256 {
		t.Fatal("replay changed installed tree")
	}

	lock := filepath.Join(firstRoot, "locks", "codex", "package-lock.json")
	if err := os.WriteFile(lock, []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	thirdRoot := t.TempDir()
	if _, err := PrepareHarnesses(context.Background(), HarnessInstallConfig{
		Mode: SourceManifest, RunRoot: thirdRoot, CacheRoot: filepath.Join(thirdRoot, "cache"), EvidenceDir: filepath.Join(thirdRoot, "evidence"),
		Harnesses: []string{"codex"}, Replay: &first.Manifest, ReplayRoot: firstRoot, RunCommand: fakeNPMInstall,
	}); err == nil || !strings.Contains(err.Error(), "lockfile hash mismatch") {
		t.Fatalf("tampered replay error = %v", err)
	}
}

func fakeNPMInstall(_ context.Context, dir string, _ map[string]string, _ io.Writer, argv ...string) error {
	if len(argv) < 2 || argv[0] != "npm" {
		return fmt.Errorf("unexpected command: %v", argv)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return err
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return err
	}
	var packageName, version string
	for packageName, version = range pkg.Dependencies {
	}
	if argv[1] == "install" {
		lock := map[string]any{
			"name": "fixture", "lockfileVersion": 3,
			"packages": map[string]any{
				"":                            map[string]any{"dependencies": pkg.Dependencies},
				"node_modules/" + packageName: map[string]any{"version": version, "resolved": "https://registry.example/fixture.tgz", "integrity": "sha512-fixture"},
			},
		}
		return writeJSON(filepath.Join(dir, "package-lock.json"), lock)
	}
	if argv[1] != "ci" {
		return fmt.Errorf("unexpected npm command: %v", argv)
	}
	binDir := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return err
	}
	executable := map[string]string{"@openai/codex": "codex"}[packageName]
	if executable == "" {
		return fmt.Errorf("unknown fixture package %q", packageName)
	}
	return os.WriteFile(filepath.Join(binDir, executable), []byte("#!/bin/sh\necho fixture\n"), 0o700)
}

func TestHarnessManifestRejectsTraversalAndIncompleteClosure(t *testing.T) {
	base := HarnessManifest{SchemaVersion: HarnessManifestSchemaVersion, CreatedAt: time.Now(), Artifacts: []HarnessArtifact{{
		Harness: "codex", Ecosystem: "npm", Package: "@openai/codex", Version: "1.0.0", Lockfile: "locks/codex/package-lock.json",
		LockfileSHA256: "a", InstalledSHA256: "b", ExecutableSHA256: "c",
		RegistryIdentities: []string{"registry.npmjs.org"}, InstallCommand: []string{"npm", "ci"},
	}}}
	if err := validateHarnessManifest(base); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.Artifacts = append([]HarnessArtifact(nil), base.Artifacts...)
	bad.Artifacts[0].Lockfile = "../../secret"
	if err := validateHarnessManifest(bad); err == nil {
		t.Fatal("manifest traversal accepted")
	}

	lock := filepath.Join(t.TempDir(), "package-lock.json")
	if err := os.WriteFile(lock, []byte(`{"lockfileVersion":3,"packages":{"":{"dependencies":{"demo":"1.0.0"}},"node_modules/demo":{"version":"1.0.0","resolved":"https://example/demo.tgz","integrity":"sha512-ok"},"node_modules/transitive":{"version":"2.0.0"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyNPMLock(lock, "demo", "1.0.0"); err == nil {
		t.Fatal("unverified transitive accepted")
	}
}

func TestVerifyNPMLockRecordsOnlyShrinkwrapIntegrityExceptions(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "package-lock.json")
	raw := `{"lockfileVersion":3,"packages":{"":{"dependencies":{"demo":"1.0.0"}},"node_modules/demo":{"version":"1.0.0","resolved":"https://registry.example/demo.tgz","integrity":"sha512-root","hasShrinkwrap":true},"node_modules/demo/node_modules/child":{"version":"2.0.0","resolved":"https://registry.example/child.tgz"}}}`
	if err := os.WriteFile(lock, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	exceptions, err := verifyNPMLock(lock, "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(exceptions) != 1 || exceptions[0] != "node_modules/demo/node_modules/child" {
		t.Fatalf("integrity exceptions = %v", exceptions)
	}
}

func TestHashInstalledTreeIsPathIndependentAndSemantic(t *testing.T) {
	makeTree := func(root string, executable bool) {
		t.Helper()
		mode := os.FileMode(0o600)
		if executable {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(root, "launcher"), []byte("root="+root), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "__pycache__"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "__pycache__", "x.pyc"), []byte(time.Now().String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "Library", "Caches"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "Library", "Caches", "download"), []byte(time.Now().String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, b := t.TempDir(), t.TempDir()
	makeTree(a, false)
	makeTree(b, false)
	ha, err := HashInstalledTree(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := HashInstalledTree(b)
	if ha != hb {
		t.Fatalf("path-independent hashes differ: %s %s", ha, hb)
	}
	if err := os.Chmod(filepath.Join(b, "launcher"), 0o700); err != nil {
		t.Fatal(err)
	}
	hb, _ = HashInstalledTree(b)
	if ha == hb {
		t.Fatal("executable bit was ignored")
	}
}

func TestHashInstalledTreeIgnoresDerivedPythonInstallerMetadata(t *testing.T) {
	makeTree := func(root, record, cache string) {
		t.Helper()
		distInfo := filepath.Join(root, "lib", "demo-1.0.dist-info")
		if err := os.MkdirAll(distInfo, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "lib", "demo.py"), []byte("value = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(distInfo, "RECORD"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(distInfo, "uv_cache.json"), []byte(cache), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, b := t.TempDir(), t.TempDir()
	makeTree(a, "prefix-a digest", `{"cache":"a"}`)
	makeTree(b, "prefix-b digest", `{"cache":"b"}`)
	ha, err := HashInstalledTree(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := HashInstalledTree(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("derived installer metadata changed hash: %s %s", ha, hb)
	}
	if err := os.WriteFile(filepath.Join(b, "lib", "demo.py"), []byte("value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hb, err = HashInstalledTree(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha == hb {
		t.Fatal("runtime-affecting Python module change was ignored")
	}
}

func TestFinishHarnessArtifactSeparatesArtifactAndLayoutRoots(t *testing.T) {
	layoutRoot := t.TempDir()
	artifactRoot := filepath.Join(layoutRoot, "hermes-agent")
	binary := filepath.Join(artifactRoot, "venv", "bin", "hermes")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(layoutRoot, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layoutRoot, "home", "installer-only"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	artifact, gotBinary, err := finishHarnessArtifact(HarnessInstallConfig{Mode: SourceLatest}, HarnessArtifact{Harness: "hermes-agent"}, artifactRoot, layoutRoot, binary, HarnessArtifact{})
	if err != nil {
		t.Fatal(err)
	}
	if gotBinary != binary {
		t.Fatalf("binary = %q, want %q", gotBinary, binary)
	}
	wantExecutable := "runtime/harnesses/hermes-agent/hermes-agent/venv/bin/hermes"
	if artifact.Executable != wantExecutable {
		t.Fatalf("executable = %q, want %q", artifact.Executable, wantExecutable)
	}
	wantTree, err := HashInstalledTree(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.InstalledSHA256 != wantTree {
		t.Fatalf("installed tree = %q, want %q", artifact.InstalledSHA256, wantTree)
	}
}

func TestInstalledHashesNormalizeResolvedInstallRootOnly(t *testing.T) {
	makeInstall := func(root string, semantic string) (string, string) {
		t.Helper()
		actualBase := filepath.Join(root, "actual-base")
		actual := filepath.Join(actualBase, "install")
		if err := os.MkdirAll(filepath.Join(actual, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		linkedBase := filepath.Join(root, "linked-base")
		if err := os.Symlink(actualBase, linkedBase); err != nil {
			t.Fatal(err)
		}
		linked := filepath.Join(linkedBase, "install")
		resolved, err := filepath.EvalSymlinks(linked)
		if err != nil {
			t.Fatal(err)
		}
		launcher := filepath.Join(actual, "bin", "demo")
		body := "#!/bin/sh\nexec " + resolved + "/bin/runtime " + semantic + "\n"
		if err := os.WriteFile(launcher, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return linked, filepath.Join(linked, "bin", "demo")
	}

	rootA, executableA := makeInstall(t.TempDir(), "stable")
	rootB, executableB := makeInstall(t.TempDir(), "stable")
	treeA, err := HashInstalledTree(rootA)
	if err != nil {
		t.Fatal(err)
	}
	treeB, err := HashInstalledTree(rootB)
	if err != nil {
		t.Fatal(err)
	}
	executableHashA, err := HashInstalledExecutable(executableA, rootA)
	if err != nil {
		t.Fatal(err)
	}
	executableHashB, err := HashInstalledExecutable(executableB, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if treeA != treeB || executableHashA != executableHashB {
		t.Fatalf("relocated hashes differ: tree %s/%s executable %s/%s", treeA, treeB, executableHashA, executableHashB)
	}
	if err := os.WriteFile(executableB, []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	changed, err := HashInstalledExecutable(executableB, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if changed == executableHashA {
		t.Fatal("semantic executable change was ignored")
	}
}

func TestInstallEnvironmentUsesSystemTrustWithoutAmbientCredentials(t *testing.T) {
	cfg := HarnessInstallConfig{RunRoot: "/run", CacheRoot: "/cache"}
	env := installEnvironment(cfg, "hermes-agent")
	if env["UV_SYSTEM_CERTS"] != "1" {
		t.Fatalf("UV_SYSTEM_CERTS = %q", env["UV_SYSTEM_CERTS"])
	}
	for _, key := range []string{"OPENAI_API_KEY", "AWS_ACCESS_KEY_ID", "SSH_AUTH_SOCK", "GIT_ASKPASS"} {
		if _, ok := env[key]; ok {
			t.Fatalf("install environment contains ambient credential channel %s", key)
		}
	}
	hermes := hermesInstallEnvironment(cfg)
	if hermes["GIT_CONFIG_KEY_0"] != "url.https://github.com/.insteadOf" || hermes["GIT_CONFIG_VALUE_0"] != "git@github.com:" {
		t.Fatalf("Hermes public clone is not forced through HTTPS: %v", hermes)
	}
}

func TestPrepareOfficialCodexHarnessLive(t *testing.T) {
	if os.Getenv("OV_TEST_LIVE_PACKAGE_INSTALL") != "1" {
		t.Skip("set OV_TEST_LIVE_PACKAGE_INSTALL=1 to exercise the official registry")
	}
	root := t.TempDir()
	prepared, err := PrepareHarnesses(context.Background(), HarnessInstallConfig{
		Mode: SourceLatest, RunRoot: root, CacheRoot: filepath.Join(root, "cache"), EvidenceDir: filepath.Join(root, "evidence"), Harnesses: []string{"codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := prepared.Manifest.Artifacts[0]
	if artifact.Version == "" || artifact.LockfileSHA256 == "" || artifact.InstalledSHA256 == "" || len(artifact.RegistryIdentities) == 0 {
		t.Fatalf("incomplete artifact provenance: %+v", artifact)
	}
	if _, err := exec.Command(prepared.Binaries["codex"], "--version").CombinedOutput(); err != nil {
		t.Fatalf("installed Codex executable: %v", err)
	}
}

func TestPrepareOfficialPiHarnessLive(t *testing.T) {
	if os.Getenv("OV_TEST_LIVE_PI_INSTALL") != "1" {
		t.Skip("set OV_TEST_LIVE_PI_INSTALL=1 to exercise Pi's published shrinkwrap")
	}
	root := t.TempDir()
	prepared, err := PrepareHarnesses(context.Background(), HarnessInstallConfig{
		Mode: SourceLatest, RunRoot: root, CacheRoot: filepath.Join(root, "cache"), EvidenceDir: filepath.Join(root, "evidence"), Harnesses: []string{"pi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := prepared.Manifest.Artifacts[0]
	if len(artifact.IntegrityExceptions) == 0 || artifact.InstalledSHA256 == "" || artifact.ExecutableSHA256 == "" {
		t.Fatalf("incomplete Pi shrinkwrap provenance: %+v", artifact)
	}
	if _, err := exec.Command(prepared.Binaries["pi"], "--version").CombinedOutput(); err != nil {
		t.Fatalf("installed Pi executable: %v", err)
	}
}

func TestPrepareOfficialHermesHarnessLive(t *testing.T) {
	if os.Getenv("OV_TEST_LIVE_HERMES_INSTALL") != "1" {
		t.Skip("set OV_TEST_LIVE_HERMES_INSTALL=1 to exercise the official installer")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	prepared, err := PrepareHarnesses(ctx, HarnessInstallConfig{
		Mode: SourceLatest, RunRoot: root, CacheRoot: filepath.Join(root, "cache"), EvidenceDir: filepath.Join(root, "evidence"), Harnesses: []string{"hermes-agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := prepared.Manifest.Artifacts[0]
	if artifact.SourceCommit == "" || artifact.InstallerSHA256 == "" || artifact.LockfileSHA256 == "" || artifact.InstalledSHA256 == "" {
		t.Fatalf("incomplete Hermes provenance: %+v", artifact)
	}
	versionCtx, versionCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer versionCancel()
	if _, err := exec.CommandContext(versionCtx, prepared.Binaries["hermes-agent"], "--version").CombinedOutput(); err != nil {
		t.Fatalf("installed Hermes executable: %v", err)
	}
}
