package releasegate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	HarnessManifestSchemaVersion = 1
	hermesRepositoryURL          = "https://github.com/NousResearch/hermes-agent.git"
	hermesInstallerURL           = "https://hermes-agent.nousresearch.com/install.sh"
)

type HarnessArtifact struct {
	Harness             string   `json:"harness"`
	Ecosystem           string   `json:"ecosystem"`
	Package             string   `json:"package,omitempty"`
	Version             string   `json:"version"`
	SourceCommit        string   `json:"source_commit,omitempty"`
	Lockfile            string   `json:"lockfile"`
	LockfileSHA256      string   `json:"lockfile_sha256"`
	Installer           string   `json:"installer,omitempty"`
	InstallerSHA256     string   `json:"installer_sha256,omitempty"`
	IntegrityExceptions []string `json:"lock_integrity_exceptions,omitempty"`
	InstalledSHA256     string   `json:"installed_tree_sha256"`
	Executable          string   `json:"executable"`
	ExecutableSHA256    string   `json:"executable_sha256"`
	RegistryIdentities  []string `json:"registry_identities"`
	InstallCommand      []string `json:"install_command"`
}

type HarnessManifest struct {
	SchemaVersion int               `json:"schema_version"`
	CreatedAt     time.Time         `json:"created_at"`
	Artifacts     []HarnessArtifact `json:"artifacts"`
}

type HarnessInstallConfig struct {
	Mode        SourceMode
	RunRoot     string
	CacheRoot   string
	EvidenceDir string
	Harnesses   []string
	Replay      *HarnessManifest
	ReplayRoot  string
	RunCommand  HarnessCommandRunner
	ResolveNPM  func(context.Context, string) (string, error)
	ResolveGit  func(context.Context, string, string) (string, error)
	Download    func(context.Context, string) ([]byte, error)
	Progress    func(harness, phase string)
}

type HarnessCommandRunner func(context.Context, string, map[string]string, io.Writer, ...string) error

type PreparedHarnesses struct {
	Manifest HarnessManifest
	Binaries map[string]string
}

type npmHarness struct {
	Name       string
	Package    string
	Executable string
}

type npmLockPackage struct {
	Version       string `json:"version"`
	Resolved      string `json:"resolved"`
	Integrity     string `json:"integrity"`
	HasShrinkwrap bool   `json:"hasShrinkwrap"`
	Link          bool   `json:"link"`
}

var officialNPMHarnesses = map[string]npmHarness{
	"claude-code": {Name: "claude-code", Package: "@anthropic-ai/claude-code", Executable: "claude"},
	"codex":       {Name: "codex", Package: "@openai/codex", Executable: "codex"},
	"openclaw":    {Name: "openclaw", Package: "openclaw", Executable: "openclaw"},
	"opencode":    {Name: "opencode", Package: "opencode-ai", Executable: "opencode"},
	"pi":          {Name: "pi", Package: "@earendil-works/pi-coding-agent", Executable: "pi"},
}

// PrepareHarnesses installs only the harnesses needed by the selected suite.
// Latest mode resolves each official stable channel once. Manifest mode never
// resolves a channel: it reuses the exact lock and root version previously
// recorded, then verifies that the installed tree is byte-for-byte equivalent.
func PrepareHarnesses(ctx context.Context, cfg HarnessInstallConfig) (*PreparedHarnesses, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Mode != SourceLatest && cfg.Mode != SourceManifest {
		return nil, fmt.Errorf("harness install: unsupported mode %q", cfg.Mode)
	}
	if cfg.RunRoot == "" || cfg.CacheRoot == "" || cfg.EvidenceDir == "" {
		return nil, fmt.Errorf("harness install: run root, cache root, and evidence directory are required")
	}
	if cfg.Mode == SourceManifest && (cfg.Replay == nil || cfg.ReplayRoot == "") {
		return nil, fmt.Errorf("harness install: manifest replay data is required")
	}
	if cfg.RunCommand == nil {
		cfg.RunCommand = runHarnessCommand
	}
	if cfg.ResolveNPM == nil {
		cfg.ResolveNPM = resolveNPMVersion
	}
	if cfg.ResolveGit == nil {
		cfg.ResolveGit = resolveGitRef
	}
	if cfg.Download == nil {
		cfg.Download = downloadArtifact
	}
	names, err := normalizeHarnessNames(cfg.Harnesses)
	if err != nil {
		return nil, err
	}
	replay := map[string]HarnessArtifact{}
	if cfg.Replay != nil {
		if err := validateHarnessManifest(*cfg.Replay); err != nil {
			return nil, err
		}
		for _, artifact := range cfg.Replay.Artifacts {
			replay[artifact.Harness] = artifact
		}
	}
	prepared := &PreparedHarnesses{Manifest: HarnessManifest{SchemaVersion: HarnessManifestSchemaVersion, CreatedAt: time.Now().UTC()}, Binaries: map[string]string{}}
	for _, name := range names {
		reportHarnessProgress(cfg.Progress, name, "acquiring")
		var artifact HarnessArtifact
		var binary string
		if name == "hermes-agent" {
			artifact, binary, err = prepareHermes(ctx, cfg, replay[name])
		} else {
			artifact, binary, err = prepareNPMHarness(ctx, cfg, officialNPMHarnesses[name], replay[name])
		}
		if err != nil {
			return nil, fmt.Errorf("install %s: %w", name, err)
		}
		prepared.Manifest.Artifacts = append(prepared.Manifest.Artifacts, artifact)
		prepared.Binaries[name] = binary
		reportHarnessProgress(cfg.Progress, name, "verified")
	}
	return prepared, nil
}

func reportHarnessProgress(progress func(harness, phase string), harness, phase string) {
	if progress != nil {
		progress(harness, phase)
	}
}

func normalizeHarnessNames(names []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if name != "hermes-agent" {
			if _, ok := officialNPMHarnesses[name]; !ok {
				return nil, fmt.Errorf("harness install: unsupported harness %q", name)
			}
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func prepareNPMHarness(ctx context.Context, cfg HarnessInstallConfig, spec npmHarness, pinned HarnessArtifact) (HarnessArtifact, string, error) {
	lockDir := filepath.Join(cfg.RunRoot, "locks", spec.Name)
	installDir := filepath.Join(cfg.RunRoot, "runtime", "harnesses", spec.Name)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return HarnessArtifact{}, "", err
	}
	version := pinned.Version
	if cfg.Mode == SourceLatest {
		var err error
		version, err = cfg.ResolveNPM(ctx, spec.Package)
		if err != nil {
			return HarnessArtifact{}, "", err
		}
		if !safeVersion(version) {
			return HarnessArtifact{}, "", fmt.Errorf("registry returned unsafe version %q", version)
		}
		pkg := map[string]any{"name": "ovtest-" + spec.Name, "private": true, "version": "0.0.0", "dependencies": map[string]string{spec.Package: version}}
		if err := writeJSON(filepath.Join(lockDir, "package.json"), pkg); err != nil {
			return HarnessArtifact{}, "", err
		}
		log, closeLog, err := newHarnessLog(cfg.EvidenceDir, "install-"+spec.Name+"-lock.log")
		if err != nil {
			return HarnessArtifact{}, "", err
		}
		err = cfg.RunCommand(ctx, lockDir, installEnvironment(cfg, spec.Name), log, "npm", "install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund")
		closeErr := closeLog()
		if err != nil || closeErr != nil {
			return HarnessArtifact{}, "", errors.Join(err, closeErr)
		}
	} else {
		if pinned.Harness != spec.Name || pinned.Package != spec.Package || pinned.Ecosystem != "npm" || !safeVersion(version) {
			return HarnessArtifact{}, "", fmt.Errorf("manifest does not contain the expected official package")
		}
		for _, filename := range []string{"package.json", "package-lock.json"} {
			if err := copyVerifiedFile(filepath.Join(cfg.ReplayRoot, filepath.FromSlash(filepath.Join(pinned.Lockfile, "..", filename))), filepath.Join(lockDir, filename), ""); err != nil {
				return HarnessArtifact{}, "", err
			}
		}
	}
	lockPath := filepath.Join(lockDir, "package-lock.json")
	lockHash, err := FileSHA256(lockPath)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	if cfg.Mode == SourceManifest && lockHash != pinned.LockfileSHA256 {
		return HarnessArtifact{}, "", fmt.Errorf("lockfile hash mismatch")
	}
	integrityExceptions, err := verifyNPMLock(lockPath, spec.Package, version)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	registries, err := npmLockRegistries(lockPath)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		return HarnessArtifact{}, "", err
	}
	for _, filename := range []string{"package.json", "package-lock.json"} {
		if err := copyVerifiedFile(filepath.Join(lockDir, filename), filepath.Join(installDir, filename), ""); err != nil {
			return HarnessArtifact{}, "", err
		}
	}
	log, closeLog, err := newHarnessLog(cfg.EvidenceDir, "install-"+spec.Name+".log")
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	err = cfg.RunCommand(ctx, installDir, installEnvironment(cfg, spec.Name), log, "npm", "ci", "--no-audit", "--no-fund")
	closeErr := closeLog()
	if err != nil || closeErr != nil {
		return HarnessArtifact{}, "", errors.Join(err, closeErr)
	}
	binary := filepath.Join(installDir, "node_modules", ".bin", spec.Executable)
	if runtime.GOOS == "windows" {
		binary += ".cmd"
	}
	return finishHarnessArtifact(cfg, HarnessArtifact{
		Harness: spec.Name, Ecosystem: "npm", Package: spec.Package, Version: version,
		Lockfile: filepath.ToSlash(filepath.Join("locks", spec.Name, "package-lock.json")), LockfileSHA256: lockHash,
		IntegrityExceptions: integrityExceptions,
		RegistryIdentities:  registries, InstallCommand: []string{"npm", "ci", "--no-audit", "--no-fund"},
	}, installDir, installDir, binary, pinned)
}

func prepareHermes(ctx context.Context, cfg HarnessInstallConfig, pinned HarnessArtifact) (HarnessArtifact, string, error) {
	lockDir := filepath.Join(cfg.RunRoot, "locks", "hermes-agent")
	installDir := filepath.Join(cfg.RunRoot, "runtime", "harnesses", "hermes-agent")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return HarnessArtifact{}, "", err
	}
	commit := pinned.SourceCommit
	installerPath := filepath.Join(lockDir, "install.sh")
	if cfg.Mode == SourceLatest {
		var err error
		commit, err = cfg.ResolveGit(ctx, hermesRepositoryURL, "refs/heads/main")
		if err != nil {
			return HarnessArtifact{}, "", err
		}
		installer, err := cfg.Download(ctx, hermesInstallerURL)
		if err != nil {
			return HarnessArtifact{}, "", err
		}
		if !bytes.HasPrefix(installer, []byte("#!")) {
			return HarnessArtifact{}, "", fmt.Errorf("official installer is not a shell script")
		}
		if err := os.WriteFile(installerPath, installer, 0o700); err != nil {
			return HarnessArtifact{}, "", err
		}
	} else {
		if pinned.Harness != "hermes-agent" || pinned.Ecosystem != "official-installer" || !fullCommit.MatchString(commit) {
			return HarnessArtifact{}, "", fmt.Errorf("manifest does not contain an exact Hermes installation")
		}
		if err := copyVerifiedFile(filepath.Join(cfg.ReplayRoot, filepath.FromSlash(pinned.Installer)), installerPath, pinned.InstallerSHA256); err != nil {
			return HarnessArtifact{}, "", err
		}
		if err := os.Chmod(installerPath, 0o700); err != nil {
			return HarnessArtifact{}, "", err
		}
	}
	installerHash, err := FileSHA256(installerPath)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	if cfg.Mode == SourceManifest && installerHash != pinned.InstallerSHA256 {
		return HarnessArtifact{}, "", fmt.Errorf("installer hash mismatch")
	}
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		return HarnessArtifact{}, "", err
	}
	repoDir := filepath.Join(installDir, "hermes-agent")
	homeDir := filepath.Join(installDir, "home")
	const hermesLogName = "install-hermes-agent.log"
	log, closeLog, err := newHarnessLog(cfg.EvidenceDir, hermesLogName)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	env := hermesInstallEnvironment(cfg)
	env["HOME"], env["HERMES_HOME"], env["HERMES_INSTALL_DIR"] = homeDir, homeDir, repoDir
	err = cfg.RunCommand(ctx, installDir, env, log, "bash", installerPath, "--skip-setup", "--skip-browser", "--no-skills", "--non-interactive", "--commit", commit, "--dir", repoDir, "--hermes-home", homeDir)
	closeErr := closeLog()
	if err != nil || closeErr != nil {
		return HarnessArtifact{}, "", errors.Join(err, closeErr)
	}
	installLog, err := os.ReadFile(filepath.Join(cfg.EvidenceDir, hermesLogName))
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	if !bytes.Contains(installLog, []byte("hash-verified via uv.lock")) {
		return HarnessArtifact{}, "", fmt.Errorf("official Hermes installer did not complete its hash-verified uv.lock tier")
	}
	lockPath := filepath.Join(repoDir, "uv.lock")
	lockHash, err := FileSHA256(lockPath)
	if err != nil {
		return HarnessArtifact{}, "", fmt.Errorf("official Hermes install did not retain uv.lock: %w", err)
	}
	retainedLock := filepath.Join(lockDir, "uv.lock")
	if err := copyVerifiedFile(lockPath, retainedLock, ""); err != nil {
		return HarnessArtifact{}, "", err
	}
	if cfg.Mode == SourceManifest && lockHash != pinned.LockfileSHA256 {
		return HarnessArtifact{}, "", fmt.Errorf("Hermes lockfile hash mismatch")
	}
	binary := filepath.Join(repoDir, "venv", "bin", "hermes")
	if runtime.GOOS == "windows" {
		binary = filepath.Join(repoDir, "venv", "Scripts", "hermes.exe")
	}
	version := pinned.Version
	if version == "" {
		version = "commit:" + commit
	}
	return finishHarnessArtifact(cfg, HarnessArtifact{
		Harness: "hermes-agent", Ecosystem: "official-installer", Version: version, SourceCommit: commit,
		Lockfile: filepath.ToSlash(filepath.Join("locks", "hermes-agent", "uv.lock")), LockfileSHA256: lockHash,
		Installer: filepath.ToSlash(filepath.Join("locks", "hermes-agent", "install.sh")), InstallerSHA256: installerHash,
		RegistryIdentities: []string{"github.com/NousResearch/hermes-agent", "hermes-agent.nousresearch.com", "pypi.org/simple"},
		InstallCommand:     []string{"install.sh", "--skip-setup", "--skip-browser", "--no-skills", "--non-interactive", "--commit", commit},
	}, repoDir, installDir, binary, pinned)
}

func hermesInstallEnvironment(cfg HarnessInstallConfig) map[string]string {
	env := installEnvironment(cfg, "hermes-agent")
	// The official installer tries its public GitHub SSH URL first. Rewrite that
	// transport to HTTPS so OpenSSH cannot discover operator keys from the OS
	// account home even though the subprocess HOME and SSH_AUTH_SOCK are isolated.
	env["GIT_CONFIG_COUNT"] = "1"
	env["GIT_CONFIG_KEY_0"] = "url.https://github.com/.insteadOf"
	env["GIT_CONFIG_VALUE_0"] = "git@github.com:"
	return env
}

func finishHarnessArtifact(cfg HarnessInstallConfig, artifact HarnessArtifact, artifactRoot, layoutRoot, binary string, pinned HarnessArtifact) (HarnessArtifact, string, error) {
	info, err := os.Stat(binary)
	if err != nil || info.IsDir() {
		return HarnessArtifact{}, "", fmt.Errorf("official executable unavailable at %s", binary)
	}
	executableHash, err := HashInstalledExecutable(binary, artifactRoot)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	treeHash, err := HashInstalledTree(artifactRoot)
	if err != nil {
		return HarnessArtifact{}, "", err
	}
	if cfg.Mode == SourceManifest {
		var mismatches []string
		if treeHash != pinned.InstalledSHA256 {
			mismatches = append(mismatches, fmt.Sprintf("installed tree sha256 got %s want %s", treeHash, pinned.InstalledSHA256))
		}
		if executableHash != pinned.ExecutableSHA256 {
			mismatches = append(mismatches, fmt.Sprintf("executable sha256 got %s want %s", executableHash, pinned.ExecutableSHA256))
		}
		if len(mismatches) != 0 {
			return HarnessArtifact{}, "", fmt.Errorf("installed artifact differs from manifest: %s", strings.Join(mismatches, "; "))
		}
	}
	artifact.InstalledSHA256 = treeHash
	artifact.Executable = filepath.ToSlash(filepath.Join("runtime", "harnesses", artifact.Harness, strings.TrimPrefix(binary, layoutRoot+string(filepath.Separator))))
	artifact.ExecutableSHA256 = executableHash
	return artifact, binary, nil
}

func verifyNPMLock(path, packageName, version string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		LockfileVersion int                       `json:"lockfileVersion"`
		Packages        map[string]npmLockPackage `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, err
	}
	root, ok := lock.Packages["node_modules/"+packageName]
	if lock.LockfileVersion < 2 || !ok || root.Version != version || root.Resolved == "" || root.Integrity == "" {
		return nil, fmt.Errorf("npm lock does not pin %s@%s with resolved URL and integrity", packageName, version)
	}
	var exceptions []string
	for name, item := range lock.Packages {
		if name == "" {
			continue
		}
		if item.Link || item.Resolved == "" {
			return nil, fmt.Errorf("npm lock contains unverified package %q", name)
		}
		if item.Integrity == "" {
			if !integrityPinnedShrinkwrapAncestor(name, lock.Packages) {
				return nil, fmt.Errorf("npm lock contains unverified package %q", name)
			}
			exceptions = append(exceptions, name)
		}
	}
	sort.Strings(exceptions)
	return exceptions, nil
}

func integrityPinnedShrinkwrapAncestor(name string, packages map[string]npmLockPackage) bool {
	for cursor := name; ; {
		index := strings.LastIndex(cursor, "/node_modules/")
		if index < 0 {
			return false
		}
		cursor = cursor[:index]
		ancestor, ok := packages[cursor]
		if ok && ancestor.HasShrinkwrap && ancestor.Resolved != "" && ancestor.Integrity != "" {
			return true
		}
	}
}

func npmLockRegistries(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Resolved string `json:"resolved"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	for name, item := range lock.Packages {
		if name == "" {
			continue
		}
		parsed, err := url.Parse(item.Resolved)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return nil, fmt.Errorf("npm lock contains unsafe resolved artifact %q", item.Resolved)
		}
		hosts[strings.ToLower(parsed.Host)] = true
	}
	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out, nil
}

func validateHarnessManifest(manifest HarnessManifest) error {
	if manifest.SchemaVersion != HarnessManifestSchemaVersion || manifest.CreatedAt.IsZero() {
		return fmt.Errorf("harness manifest: unsupported or incomplete")
	}
	seen := map[string]bool{}
	for _, item := range manifest.Artifacts {
		if item.Harness == "" || seen[item.Harness] || item.Version == "" || item.LockfileSHA256 == "" || item.InstalledSHA256 == "" || item.ExecutableSHA256 == "" || len(item.RegistryIdentities) == 0 || len(item.InstallCommand) == 0 || !safeManifestArtifactPath(item.Lockfile) || (item.Installer != "" && !safeManifestArtifactPath(item.Installer)) {
			return fmt.Errorf("harness manifest: invalid artifact %q", item.Harness)
		}
		seen[item.Harness] = true
	}
	return nil
}

func safeManifestArtifactPath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func resolveNPMVersion(ctx context.Context, packageName string) (string, error) {
	out, err := command(ctx, "npm", "view", packageName+"@latest", "version", "--json")
	if err != nil {
		return "", err
	}
	var version string
	if err := json.Unmarshal([]byte(out), &version); err != nil {
		return "", fmt.Errorf("decode npm stable version: %w", err)
	}
	return version, nil
}

func resolveGitRef(ctx context.Context, repository, ref string) (string, error) {
	out, err := command(ctx, "git", "ls-remote", "--exit-code", repository, ref)
	if err != nil {
		return "", err
	}
	sha := strings.Fields(out)
	if len(sha) < 2 || !fullCommit.MatchString(sha[0]) || sha[1] != ref {
		return "", fmt.Errorf("Git returned an invalid ref resolution")
	}
	return sha[0], nil
}

func downloadArtifact(ctx context.Context, rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("artifact redirect is not HTTPS")
		}
		if len(via) >= 5 {
			return fmt.Errorf("too many artifact redirects")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func runHarnessCommand(ctx context.Context, dir string, extra map[string]string, log io.Writer, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("install command is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	prepareManagedCommand(cmd)
	cmd.Cancel = func() error { return killManagedCommand(cmd) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = mergeProcessEnv(buildBaseEnv(os.Environ()), extra)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

func installEnvironment(cfg HarnessInstallConfig, name string) map[string]string {
	home := filepath.Join(cfg.RunRoot, "runtime", "install-homes", name)
	return map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"),
		"XDG_STATE_HOME": filepath.Join(home, "state"), "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"npm_config_cache": filepath.Join(cfg.CacheRoot, "npm"), "UV_CACHE_DIR": filepath.Join(cfg.CacheRoot, "uv"),
		"UV_SYSTEM_CERTS":     "1",
		"GIT_TERMINAL_PROMPT": "0", "NPM_CONFIG_FUND": "false", "NPM_CONFIG_AUDIT": "false",
	}
}

func newHarnessLog(dir, name string) (io.Writer, func() error, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func copyVerifiedFile(source, target, expectedHash string) error {
	if expectedHash != "" {
		got, err := FileSHA256(source)
		if err != nil {
			return err
		}
		if got != expectedHash {
			return fmt.Errorf("artifact hash mismatch for %s", source)
		}
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, raw, 0o600)
}

func safeVersion(version string) bool {
	if version == "" || strings.ContainsAny(version, "@/\\ \t\r\n") {
		return false
	}
	for _, r := range version {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && !strings.ContainsRune(".-+_", r) {
			return false
		}
	}
	return true
}

// HashInstalledTree hashes semantic installed state: relative paths, contents,
// symlink targets, and executable bits. It ignores only VCS data, caches,
// bytecode, logs, and OS metadata. Absolute installation-root occurrences are
// normalized so an exact replay in another disposable directory remains equal.
func HashInstalledTree(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootAliases := installedRootAliases(root)
	hash := sha256.New()
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if nonSemanticInstallArtifact(rel, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		_, _ = io.WriteString(hash, filepath.ToSlash(rel)+"\x00")
		switch {
		case entry.Type().IsRegular():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o111 != 0 {
				_, _ = io.WriteString(hash, "file+x\x00")
			} else {
				_, _ = io.WriteString(hash, "file\x00")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			raw = normalizeInstalledBytes(raw, rootAliases)
			_, _ = hash.Write(raw)
		case entry.Type()&os.ModeSymlink != 0:
			_, _ = io.WriteString(hash, "symlink\x00")
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			target = normalizeInstalledString(target, rootAliases)
			_, _ = io.WriteString(hash, target)
		case entry.IsDir():
			_, _ = io.WriteString(hash, "dir\x00")
		default:
			return fmt.Errorf("installed tree: unsupported file type at %s", rel)
		}
		_, _ = io.WriteString(hash, "\x00")
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// HashInstalledExecutable records relocatable executable content. Package
// managers commonly generate launchers containing the absolute install prefix;
// exact replay in another disposable run must normalize only that prefix while
// still hashing every other byte. HashInstalledTree separately records the
// executable path, symlink target, and executable bit.
func HashInstalledExecutable(path, installRoot string) (string, error) {
	root, err := filepath.Abs(installRoot)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	normalized := normalizeInstalledBytes(raw, installedRootAliases(root))
	digest := sha256.Sum256(normalized)
	return hex.EncodeToString(digest[:]), nil
}

func installedRootAliases(root string) []string {
	seen := map[string]bool{}
	var aliases []string
	add := func(value string) {
		value = filepath.Clean(value)
		for _, candidate := range []string{value, filepath.ToSlash(value)} {
			if candidate != "" && candidate != "." && !seen[candidate] {
				seen[candidate] = true
				aliases = append(aliases, candidate)
			}
		}
	}
	add(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		add(resolved)
	}
	sort.Slice(aliases, func(i, j int) bool { return len(aliases[i]) > len(aliases[j]) })
	return aliases
}

func normalizeInstalledBytes(raw []byte, rootAliases []string) []byte {
	for _, root := range rootAliases {
		raw = bytes.ReplaceAll(raw, []byte(root), []byte("${INSTALL_ROOT}"))
	}
	return raw
}

func normalizeInstalledString(value string, rootAliases []string) string {
	for _, root := range rootAliases {
		value = strings.ReplaceAll(value, root, "${INSTALL_ROOT}")
	}
	return value
}

func nonSemanticInstallArtifact(rel string, directory bool) bool {
	base := filepath.Base(rel)
	if base == ".DS_Store" || base == "__pycache__" || base == ".git" || base == ".cache" || base == "logs" ||
		(directory && (strings.EqualFold(base, "cache") || strings.EqualFold(base, "caches"))) {
		return true
	}
	if directory {
		return false
	}
	// Python installers regenerate RECORD hashes for prefix-bearing launchers
	// and uv_cache.json for their local cache bookkeeping. The launchers and all
	// runtime files are hashed independently, so retaining these derived values
	// would make an otherwise exact installation depend on its absolute prefix.
	parent := filepath.Base(filepath.Dir(rel))
	if strings.HasSuffix(parent, ".dist-info") && (base == "RECORD" || base == "uv_cache.json") {
		return true
	}
	return strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, ".pyo") || strings.HasSuffix(base, ".log")
}
