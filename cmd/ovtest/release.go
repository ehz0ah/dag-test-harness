package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	claudeadapter "code.byted.org/data-arch/ovtest/adapters/claude"
	codexadapter "code.byted.org/data-arch/ovtest/adapters/codex"
	hermesadapter "code.byted.org/data-arch/ovtest/adapters/hermes"
	openclawadapter "code.byted.org/data-arch/ovtest/adapters/openclaw"
	opencodeadapter "code.byted.org/data-arch/ovtest/adapters/opencode"
	piadapter "code.byted.org/data-arch/ovtest/adapters/pi"
	"code.byted.org/data-arch/ovtest/cases"
	"code.byted.org/data-arch/ovtest/dag"
	"code.byted.org/data-arch/ovtest/internal/cli"
	"code.byted.org/data-arch/ovtest/internal/releasegate"
	"code.byted.org/data-arch/ovtest/ops"
	"code.byted.org/data-arch/ovtest/ops/checks"
	"code.byted.org/data-arch/ovtest/runner"
)

type releaseOptions struct {
	SourceMode          releasegate.SourceMode
	SourceManifest      string
	OpenVikingRef       string
	TrustedExecution    bool
	TargetMode          releasegate.TargetMode
	ExternalURL         string
	EnvFile             string
	Root                string
	OpenVikingConfig    string
	Retention           time.Duration
	KeepSuccessEvidence bool
	CodexModel          string
	ClaudeModel         string
	OpenCodeModel       string
	QualificationRuns   int
	Suite               string
	CaseIDs             []string
}

func parseReleaseArgs(args []string) (releaseOptions, error) {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		cache = os.TempDir()
	}
	var opts releaseOptions
	var source, target string
	var retentionDays int
	fs := flag.NewFlagSet("ovtest release", flag.ContinueOnError)
	fs.SetOutput(new(bytes.Buffer))
	fs.StringVar(&source, "source", string(releasegate.SourceLatest), "source mode: latest or manifest")
	fs.StringVar(&opts.SourceManifest, "manifest", "", "environment manifest for exact replay")
	fs.StringVar(&opts.OpenVikingRef, "openviking-ref", "refs/heads/main", "trusted OpenViking branch, PR head/merge ref, or full commit")
	fs.BoolVar(&opts.TrustedExecution, "trusted-execution", false, "confirm candidate code may run with scoped test credentials")
	fs.StringVar(&target, "target", string(releasegate.TargetLocal), "OpenViking target: local or external")
	fs.StringVar(&opts.ExternalURL, "url", "", "external OpenViking base URL")
	fs.StringVar(&opts.EnvFile, "env-file", defaultReleaseEnvFile(), "credential file used only by OpenViking, OpenClaw, and Hermes")
	fs.StringVar(&opts.Root, "root", filepath.Join(cache, "ovtest", "release-gate"), "persistent release-gate root")
	fs.StringVar(&opts.OpenVikingConfig, "openviking-config", "", "local OpenViking JSON template override")
	fs.IntVar(&retentionDays, "retention-days", 30, "failed evidence and source-ref retention")
	fs.BoolVar(&opts.KeepSuccessEvidence, "keep-success-evidence", false, "retain non-secret evidence for successful runs")
	fs.StringVar(&opts.CodexModel, "codex-model", os.Getenv("OV_TEST_CODEX_MODEL"), "Codex model override")
	fs.StringVar(&opts.ClaudeModel, "claude-model", os.Getenv("OV_TEST_CLAUDE_MODEL"), "Claude Code model override")
	fs.StringVar(&opts.OpenCodeModel, "opencode-model", os.Getenv("OV_TEST_OPENCODE_MODEL"), "OpenCode model override")
	fs.IntVar(&opts.QualificationRuns, "qualification-runs", 1, "fresh complete attempts using one frozen environment")
	fs.StringVar(&opts.Suite, "suite", "", "named suite: openviking, hermes-agent, openclaw, codex, claude-code, opencode, pi, or smoke")
	if err := fs.Parse(args); err != nil {
		return releaseOptions{}, err
	}
	opts.SourceMode = releasegate.SourceMode(source)
	opts.TargetMode = releasegate.TargetMode(target)
	opts.CaseIDs = fs.Args()
	if opts.Suite != "" && len(opts.CaseIDs) > 0 {
		return releaseOptions{}, fmt.Errorf("--suite cannot be combined with positional case IDs")
	}
	if opts.Suite == "all" {
		opts.Suite = "openviking"
	}
	if opts.Suite == "" && len(opts.CaseIDs) == 0 {
		opts.Suite = "openviking"
	}
	if opts.Suite == "" && len(opts.CaseIDs) == 1 && (opts.CaseIDs[0] == "all" || opts.CaseIDs[0] == "smoke") {
		opts.Suite = opts.CaseIDs[0]
		opts.CaseIDs = nil
		if opts.Suite == "all" {
			opts.Suite = "openviking"
		}
	}
	if opts.Suite != "" {
		var ok bool
		opts.CaseIDs, ok = cases.Suite(opts.Suite)
		if !ok {
			return releaseOptions{}, fmt.Errorf("unknown suite %q (try -list-suites)", opts.Suite)
		}
	} else {
		// Explicit case selections still need a stable gate identity and run-manifest label.
		opts.Suite = "custom"
	}
	if opts.SourceMode != releasegate.SourceLatest && opts.SourceMode != releasegate.SourceManifest {
		return releaseOptions{}, fmt.Errorf("--source must be latest or manifest")
	}
	if opts.SourceMode == releasegate.SourceManifest && strings.TrimSpace(opts.SourceManifest) == "" {
		return releaseOptions{}, fmt.Errorf("--source manifest requires --manifest")
	}
	if opts.SourceMode == releasegate.SourceLatest && opts.SourceManifest != "" {
		return releaseOptions{}, fmt.Errorf("--manifest is only valid with --source manifest")
	}
	if opts.SourceMode == releasegate.SourceManifest && opts.OpenVikingRef != "refs/heads/main" {
		return releaseOptions{}, fmt.Errorf("--openviking-ref is only valid with --source latest")
	}
	if opts.SourceMode == releasegate.SourceLatest {
		if _, err := releasegate.OpenVikingRepository(opts.OpenVikingRef); err != nil {
			return releaseOptions{}, err
		}
	}
	if opts.TargetMode != releasegate.TargetLocal && opts.TargetMode != releasegate.TargetExternal {
		return releaseOptions{}, fmt.Errorf("--target must be local or external")
	}
	if opts.TargetMode == releasegate.TargetExternal && strings.TrimSpace(opts.ExternalURL) == "" {
		return releaseOptions{}, fmt.Errorf("--target external requires --url")
	}
	if opts.TargetMode == releasegate.TargetExternal {
		parsed, parseErr := url.Parse(opts.ExternalURL)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return releaseOptions{}, fmt.Errorf("--url must be an HTTP(S) OpenViking base URL without credentials, query, or fragment")
		}
	}
	if retentionDays < 1 {
		return releaseOptions{}, fmt.Errorf("--retention-days must be positive")
	}
	if opts.QualificationRuns < 1 || opts.QualificationRuns > 20 {
		return releaseOptions{}, fmt.Errorf("--qualification-runs must be between 1 and 20")
	}
	opts.Retention = time.Duration(retentionDays) * 24 * time.Hour
	if strings.TrimSpace(opts.Root) == "" {
		return releaseOptions{}, fmt.Errorf("--root is required")
	}
	for _, id := range opts.CaseIDs {
		if _, ok := cases.All[id]; !ok {
			return releaseOptions{}, fmt.Errorf("unknown case %q (try -list)", id)
		}
	}
	return opts, nil
}

func defaultReleaseEnvFile() string {
	return strings.TrimSpace(os.Getenv("OV_TEST_ENV_FILE"))
}

func runRelease(args []string) int {
	opts, err := parseReleaseArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(releaseUsage)
			return 0
		}
		fmt.Fprintln(os.Stderr, "ovtest release:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	outcome, runDir := executeRelease(ctx, opts)
	if outcome.Primary != nil {
		fmt.Fprintln(os.Stderr, "release primary failure:", releasegate.RedactText(outcome.Primary.Error(), nil))
	}
	if outcome.Cleanup != nil {
		fmt.Fprintln(os.Stderr, "release cleanup failure:", releasegate.RedactText(outcome.Cleanup.Error(), nil))
	}
	if runDir != "" {
		fmt.Println("release artifacts:", runDir)
	}
	if outcome.Err() != nil {
		return 1
	}
	return 0
}

const releaseUsage = `Usage:
  ovtest release [flags] [all|smoke|case-id ...]

Core flags:
  --source latest|manifest   resolve official stable artifacts or replay an environment
  --manifest PATH            environment manifest required by manifest mode
  --openviking-ref REF       OpenViking branch, PR head/merge ref, or full commit
  --trusted-execution        authorize scoped credentials for candidate integration code
  --target local|external    manage a fresh local server or validate an existing one
  --suite NAME               run a named product or harness suite
  --url URL                  external OpenViking base URL
  --env-file PATH            typed OpenViking/OpenClaw/Hermes credentials
  --root PATH                persistent mirrors, caches, manifests, and retained evidence
  --retention-days N         failed evidence and protected-ref retention (default 30)
  --qualification-runs N     fresh complete attempts; release qualification uses 3

Run "ovtest -list" for case IDs.
`

func executeRelease(ctx context.Context, opts releaseOptions) (outcome releasegate.Outcome, runDir string) {
	if ctx == nil {
		ctx = context.Background()
	}
	runID, err := newReleaseRunID(time.Now().UTC())
	if err != nil {
		outcome.Primary = err
		return outcome, ""
	}
	if _, err := releasegate.PruneExpiredRuns(ctx, opts.Root, opts.Retention, time.Now().UTC()); err != nil {
		outcome.Primary = fmt.Errorf("prune expired runs: %w", err)
		return outcome, ""
	}
	artifacts, err := releasegate.NewRunArtifacts(opts.Root, runID)
	if err != nil {
		outcome.Primary = err
		return outcome, ""
	}
	runDir = artifacts.RunDir
	releaseProgress("run %s: preparing frozen environment", runID)
	var prepared, baseline *releasegate.PreparedSources
	var installed *releasegate.PreparedHarnesses
	var builds map[string]releasegate.BuildProvenance
	var credentials releasegate.RuntimeCredentials
	var redactionSecrets []string
	classification := releaseClassificationNone
	environmentManifestPath := filepath.Join(artifacts.RunDir, "environment-manifest.json")

	func() {
		if err := releasegate.ValidateBuildTools(); err != nil {
			outcome.Primary = err
			return
		}
		var replayEnvironment *releasegate.EnvironmentManifest
		if opts.SourceMode == releasegate.SourceManifest {
			replayed, readErr := releasegate.ReadEnvironmentManifest(opts.SourceManifest)
			if readErr != nil {
				outcome.Primary = fmt.Errorf("read replay environment: %w", readErr)
				return
			}
			replayEnvironment = &replayed
		}
		repositories := releasegate.DefaultRepositories()
		var replaySource *releasegate.Manifest
		if replayEnvironment != nil {
			replaySource = &replayEnvironment.Source
		} else {
			repository, repoErr := releasegate.OpenVikingRepository(opts.OpenVikingRef)
			if repoErr != nil {
				outcome.Primary = repoErr
				return
			}
			repositories = []releasegate.Repository{repository}
		}
		releaseProgress("resolving OpenViking source")
		prepared, err = releasegate.PrepareSources(ctx, releasegate.SourceConfig{
			Root: opts.Root, RunID: runID, Mode: opts.SourceMode, ManifestPath: opts.SourceManifest,
			Replay: replaySource, Repositories: repositories,
		})
		if err != nil {
			outcome.Primary = fmt.Errorf("prepare OpenViking candidate: %w", err)
			return
		}
		if !opts.TrustedExecution {
			outcome.Primary = fmt.Errorf("executing candidate source and credentialed integrations requires --trusted-execution; source was fetched but no candidate code was run")
			return
		}
		releaseProgress("OpenViking source frozen at %s", prepared.Manifest.Sources[0].Commit)
		if replayEnvironment != nil && replayEnvironment.Baseline != nil {
			baseline, err = releasegate.PrepareSources(ctx, releasegate.SourceConfig{
				Root: opts.Root, RunID: runID + "-base", Mode: releasegate.SourceManifest, Replay: replayEnvironment.Baseline,
			})
			if err != nil {
				outcome.Primary = fmt.Errorf("prepare replayed OpenViking base: %w", err)
				return
			}
		} else if opts.SourceMode == releasegate.SourceLatest && opts.TargetMode == releasegate.TargetLocal && opts.OpenVikingRef != "refs/heads/main" {
			baseRepo, _ := releasegate.OpenVikingRepository("refs/heads/main")
			baseline, err = releasegate.PrepareSources(ctx, releasegate.SourceConfig{
				Root: opts.Root, RunID: runID + "-base", Mode: releasegate.SourceLatest, Repositories: []releasegate.Repository{baseRepo},
			})
			if err != nil {
				outcome.Primary = fmt.Errorf("freeze OpenViking base: %w", err)
				return
			}
		}
		releaseProgress("building OpenViking in its disposable worktree")
		specs, specErr := releasegate.DefaultBuildSpecs(filepath.Join(opts.Root, "cache"), prepared)
		if specErr != nil {
			outcome.Primary = specErr
			return
		}
		builds, err = releasegate.BuildPreparedSources(ctx, prepared, artifacts.EvidenceDir,
			filepath.Join(artifacts.RuntimeDir, "build-homes", "candidate"), specs)
		if err != nil {
			outcome.Primary = fmt.Errorf("build OpenViking candidate: %w", err)
			return
		}
		releaseProgress("OpenViking build complete")
		var replayHarnesses *releasegate.HarnessManifest
		var replayRoot string
		if replayEnvironment != nil {
			replayHarnesses, replayRoot = &replayEnvironment.Harnesses, filepath.Dir(opts.SourceManifest)
		}
		harnessNames := requiredHarnessPackages(opts.CaseIDs)
		releaseProgress("installing official stable harnesses: %s", strings.Join(harnessNames, ", "))
		installed, err = releasegate.PrepareHarnesses(ctx, releasegate.HarnessInstallConfig{
			Mode: opts.SourceMode, RunRoot: artifacts.RunDir, CacheRoot: filepath.Join(opts.Root, "cache"), EvidenceDir: artifacts.EvidenceDir,
			Harnesses: harnessNames, Replay: replayHarnesses, ReplayRoot: replayRoot,
			Progress: func(harness, phase string) { releaseProgress("harness %s: %s", harness, phase) },
		})
		if err != nil {
			outcome.Primary = fmt.Errorf("prepare official harnesses: %w", err)
			return
		}
		releaseProgress("official harness installation and verification complete")
		credentials, err = loadReleaseCredentials(opts.EnvFile)
		if err != nil {
			outcome.Primary = err
			return
		}
		for _, warning := range credentials.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", warning)
		}
		if opts.TargetMode == releasegate.TargetLocal {
			if err := requireLocalProvider(credentials.Model); err != nil {
				outcome.Primary = err
				return
			}
		}
		if needsHarness(opts.CaseIDs, "openclaw-") || needsHarness(opts.CaseIDs, "hermes-") || needsHarness(opts.CaseIDs, "pi-") {
			if credentials.Model.LLMAPIKey == "" || credentials.Model.LLMBaseURL == "" || credentials.Model.LLMModel == "" {
				outcome.Primary = fmt.Errorf("OpenClaw, Hermes, and Pi require LLM API key, base URL, and model in --env-file")
				return
			}
		}
		environmentManifest, manifestErr := buildEnvironmentManifest(ctx, runID, opts, prepared, baseline, installed, credentials.Model)
		if manifestErr != nil {
			outcome.Primary = manifestErr
			return
		}
		if replayEnvironment != nil {
			if err := releasegate.ValidateEnvironmentReplay(environmentManifest, *replayEnvironment); err != nil {
				outcome.Primary = err
				return
			}
		}
		if err := releasegate.WriteEnvironmentManifest(environmentManifestPath, environmentManifest); err != nil {
			outcome.Primary = err
			return
		}

		for attempt := 1; attempt <= opts.QualificationRuns; attempt++ {
			label := fmt.Sprintf("candidate-%d", attempt)
			releaseProgress("starting %s with fresh OpenViking and harness state", label)
			result := executeReleaseAttempt(ctx, releaseAttemptConfig{
				RunID: runID, Label: label, Options: opts, RootArtifacts: artifacts, Prepared: prepared, Builds: builds,
				Installed: installed, Credentials: credentials, EnvironmentManifestPath: environmentManifestPath, CaseIDs: opts.CaseIDs,
			})
			redactionSecrets = append(redactionSecrets, result.Secrets...)
			if result.Outcome.Err() == nil {
				continue
			}
			outcome = result.Outcome
			if result.Class != releaseFailureCase || baseline == nil || len(result.FailedCases) == 0 {
				classification = classificationWithoutComparison(result)
				return
			}
			releaseProgress("candidate case failure; building frozen base for comparison")
			baseSpecs, baseSpecErr := releasegate.DefaultBuildSpecs(filepath.Join(opts.Root, "cache"), baseline)
			if baseSpecErr != nil {
				outcome.Primary = errors.Join(outcome.Primary, fmt.Errorf("prepare base comparison: %w", baseSpecErr))
				return
			}
			baseBuilds, baseBuildErr := releasegate.BuildPreparedSources(ctx, baseline,
				filepath.Join(artifacts.EvidenceDir, "base-build"),
				filepath.Join(artifacts.RuntimeDir, "build-homes", "base"), baseSpecs)
			if baseBuildErr != nil {
				outcome.Primary = errors.Join(outcome.Primary, fmt.Errorf("build base comparison: %w", baseBuildErr))
				return
			}
			baseOpts := opts
			baseOpts.CaseIDs, baseOpts.QualificationRuns = append([]string(nil), result.FailedCases...), 1
			base := executeReleaseAttempt(ctx, releaseAttemptConfig{
				RunID: runID, Label: "base-comparison", Options: baseOpts, RootArtifacts: artifacts, Prepared: baseline, Builds: baseBuilds,
				Installed: installed, Credentials: credentials, EnvironmentManifestPath: environmentManifestPath, CaseIDs: result.FailedCases,
			})
			redactionSecrets = append(redactionSecrets, base.Secrets...)
			if base.Outcome.Err() != nil && base.Class != releaseFailureCase {
				comparison := classifyCandidateComparison(result, base, releaseAttemptResult{})
				outcome, classification = comparison.Outcome, comparison.Classification
				return
			}
			candidateOnlyCases := failedCaseDifference(result.FailedCases, base.FailedCases)
			if len(candidateOnlyCases) == 0 {
				comparison := classifyCandidateComparison(result, base, releaseAttemptResult{})
				outcome, classification = comparison.Outcome, comparison.Classification
				return
			}
			releaseProgress("base comparison isolated candidate-only failures; retrying them in fresh state")
			baseOpts.CaseIDs = candidateOnlyCases
			retry := executeReleaseAttempt(ctx, releaseAttemptConfig{
				RunID: runID, Label: "candidate-retry", Options: baseOpts, RootArtifacts: artifacts, Prepared: prepared, Builds: builds,
				Installed: installed, Credentials: credentials, EnvironmentManifestPath: environmentManifestPath, CaseIDs: candidateOnlyCases,
			})
			redactionSecrets = append(redactionSecrets, retry.Secrets...)
			comparison := classifyCandidateComparison(result, base, retry)
			outcome, classification = comparison.Outcome, comparison.Classification
			return
		}
	}()

	for name, source := range map[string]*releasegate.PreparedSources{"candidate": prepared, "base": baseline} {
		if source == nil {
			continue
		}
		worktreeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := source.Close(worktreeCtx); err != nil {
			outcome.Cleanup = errors.Join(outcome.Cleanup, fmt.Errorf("remove %s worktree: %w", name, err))
		}
		cancel()
	}
	success := outcome.Err() == nil
	redactionSecrets = append(redactionSecrets, releaseSecrets(credentials, nil)...)
	if err := releasegate.RedactEvidenceTree(artifacts.EvidenceDir, redactionSecrets); err != nil {
		outcome.Cleanup = errors.Join(outcome.Cleanup, fmt.Errorf("redact retained evidence: %w", err))
		success = false
	}
	if err := artifacts.Finalize(success, opts.KeepSuccessEvidence); err != nil {
		outcome.Cleanup = errors.Join(outcome.Cleanup, fmt.Errorf("finalize artifacts: %w", err))
	}
	if outcome.Cleanup != nil || (outcome.Primary != nil && classification == releaseClassificationNone) {
		classification = releaseClassificationEnvironmentFailure
	}
	if err := writeReleaseSummary(filepath.Join(artifacts.RunDir, "run-summary.json"), runID, outcome, classification, releaseSecrets(credentials, nil)); err != nil {
		outcome.Cleanup = errors.Join(outcome.Cleanup, fmt.Errorf("write release summary: %w", err))
	}
	if outcome.Err() == nil {
		releaseProgress("run %s passed", runID)
	}
	return outcome, runDir
}

func releaseProgress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[release] "+format+"\n", args...)
}

type releaseFailureClass string

const (
	releaseFailureNone        releaseFailureClass = "none"
	releaseFailureCase        releaseFailureClass = "case"
	releaseFailureEnvironment releaseFailureClass = "environment"
)

type releaseClassification string

const (
	releaseClassificationNone                  releaseClassification = "none"
	releaseClassificationUncomparedCaseFailure releaseClassification = "uncompared-case-failure"
	releaseClassificationBaselineIncompatible  releaseClassification = "baseline-incompatible"
	releaseClassificationProbableRegression    releaseClassification = "probable-openviking-regression"
	releaseClassificationFlakyInconclusive     releaseClassification = "flaky-inconclusive"
	releaseClassificationEnvironmentFailure    releaseClassification = "environment-failure"
)

type releaseAttemptResult struct {
	Outcome     releasegate.Outcome
	Class       releaseFailureClass
	FailedCases []string
	Secrets     []string
}

type releaseAttemptConfig struct {
	RunID                   string
	Label                   string
	Options                 releaseOptions
	RootArtifacts           *releasegate.RunArtifacts
	Prepared                *releasegate.PreparedSources
	Builds                  map[string]releasegate.BuildProvenance
	Installed               *releasegate.PreparedHarnesses
	Credentials             releasegate.RuntimeCredentials
	EnvironmentManifestPath string
	CaseIDs                 []string
}

type releaseComparisonResult struct {
	Outcome        releasegate.Outcome
	Classification releaseClassification
}

func classificationWithoutComparison(result releaseAttemptResult) releaseClassification {
	if result.Class == releaseFailureCase {
		return releaseClassificationUncomparedCaseFailure
	}
	if result.Outcome.Err() != nil {
		return releaseClassificationEnvironmentFailure
	}
	return releaseClassificationNone
}

func failedCaseDifference(candidate, baseline []string) []string {
	baselineSet := make(map[string]struct{}, len(baseline))
	for _, id := range baseline {
		baselineSet[id] = struct{}{}
	}
	result := make([]string, 0, len(candidate))
	for _, id := range candidate {
		if _, failedOnBaseline := baselineSet[id]; !failedOnBaseline {
			result = append(result, id)
		}
	}
	return result
}

func classifyCandidateComparison(initial, base, retry releaseAttemptResult) releaseComparisonResult {
	out := initial.Outcome
	out.Cleanup = errors.Join(out.Cleanup, base.Outcome.Cleanup, retry.Outcome.Cleanup)
	if base.Outcome.Err() != nil {
		if base.Class == releaseFailureCase && len(failedCaseDifference(initial.FailedCases, base.FailedCases)) == 0 {
			out.Primary = errors.Join(out.Primary, fmt.Errorf("classification baseline-incompatible: the frozen base also failed: %w", base.Outcome.Primary))
			return releaseComparisonResult{Outcome: out, Classification: releaseClassificationBaselineIncompatible}
		}
		if base.Class != releaseFailureCase {
			out.Primary = errors.Join(out.Primary, fmt.Errorf("classification environment-failure: base comparison could not complete: %w", base.Outcome.Err()))
			return releaseComparisonResult{Outcome: out, Classification: releaseClassificationEnvironmentFailure}
		}
		out.Primary = errors.Join(out.Primary, fmt.Errorf("the frozen base also failed these baseline-incompatible cases: %w", base.Outcome.Primary))
	}
	if retry.Outcome.Err() != nil {
		if retry.Class == releaseFailureCase {
			out.Primary = errors.Join(out.Primary, fmt.Errorf("classification probable-openviking-regression: base passed and candidate failed twice: %w", retry.Outcome.Primary))
			return releaseComparisonResult{Outcome: out, Classification: releaseClassificationProbableRegression}
		} else {
			out.Primary = errors.Join(out.Primary, fmt.Errorf("classification environment-failure: candidate retry could not complete: %w", retry.Outcome.Err()))
			return releaseComparisonResult{Outcome: out, Classification: releaseClassificationEnvironmentFailure}
		}
	}
	out.Primary = errors.Join(out.Primary, fmt.Errorf("classification flaky-inconclusive: base and candidate retry passed after the initial candidate failure"))
	return releaseComparisonResult{Outcome: out, Classification: releaseClassificationFlakyInconclusive}
}

func executeReleaseAttempt(ctx context.Context, cfg releaseAttemptConfig) (result releaseAttemptResult) {
	result.Class = releaseFailureNone
	artifacts, err := releasegate.NewAttemptArtifacts(cfg.RootArtifacts, cfg.Label)
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	recordsPath := filepath.Join(cfg.RootArtifacts.RunDir, "results.jsonl")
	runManifestPath := filepath.Join(cfg.RootArtifacts.RunDir, "run-manifest-"+cfg.Label+".json")
	ownership := releasegate.NewOwnershipRegistry(cfg.RunID)
	credentials := cfg.Credentials
	var local *releasegate.LocalOpenViking
	var targetURL, userKey, userConf, ovBin string

	// Register this before service/remote cleanup so LIFO defer order redacts
	// every diagnostic file after all cleanup output has been written.
	defer func() {
		for _, dir := range []string{artifacts.RuntimeDir, artifacts.EvidenceDir} {
			if err := releasegate.RedactEvidenceTree(dir, result.Secrets); err != nil {
				result.Outcome.Cleanup = errors.Join(result.Outcome.Cleanup, fmt.Errorf("redact attempt diagnostics: %w", err))
				result.Class = releaseFailureEnvironment
			}
		}
	}()
	defer func() {
		if cfg.Options.TargetMode == releasegate.TargetExternal && ovBin != "" && userConf != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			restore := applySanitizedHarnessEnvironment(nil)
			cleaned := cleanupReleaseURIs(cleanupCtx, cfg.Options.TargetMode, ownership, result.Outcome.Primary, func(deleteCtx context.Context, uri string) error {
				return deleteReleaseURI(deleteCtx, ovBin, userConf, uri)
			})
			restore()
			cancel()
			result.Outcome = cleaned
		}
		if local != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := local.Service.Stop(stopCtx, 15*time.Second); err != nil {
				result.Outcome.Cleanup = errors.Join(result.Outcome.Cleanup, fmt.Errorf("stop OpenViking: %w", err))
			}
			cancel()
		}
		if result.Outcome.Cleanup != nil {
			result.Class = releaseFailureEnvironment
		}
	}()

	serverBin, err := releasegate.BuiltArtifact(cfg.Builds, "OpenViking", "server")
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	ovBin, err = releasegate.BuiltArtifact(cfg.Builds, "OpenViking", "ov")
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	if cfg.Options.TargetMode == releasegate.TargetLocal {
		template := cfg.Options.OpenVikingConfig
		if template == "" {
			template = filepath.Join(artifacts.SecretsDir, "openviking-template.json")
			if err := releasegate.WriteDefaultLocalOpenVikingTemplate(template); err != nil {
				result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
				return result
			}
		}
		local, err = releasegate.StartLocalOpenViking(ctx, releasegate.LocalOpenVikingConfig{
			ServerBin: serverBin, TemplateConfig: template, RuntimeDir: artifacts.RuntimeDir, SecretsDir: artifacts.SecretsDir,
			EvidenceDir: artifacts.EvidenceDir, Env: credentials.OpenVikingProcess(), StartupTimeout: 5 * time.Minute,
			AccountID: "ovtest", UserID: "runner",
		})
		if err != nil {
			result.Outcome.Primary, result.Class = fmt.Errorf("start local OpenViking: %w", err), releaseFailureEnvironment
			return result
		}
		targetURL, userKey, userConf = local.URL, local.UserAPIKey, local.UserConfigPath
	} else {
		targetURL, userKey = strings.TrimRight(cfg.Options.ExternalURL, "/"), credentials.OpenViking.UserAPIKey
		if err := releasegate.ValidateExternalTarget(ctx, targetURL, userKey); err != nil {
			result.Outcome.Primary, result.Class = fmt.Errorf("external OpenViking preflight: %w", err), releaseFailureEnvironment
			return result
		}
		userConf = filepath.Join(artifacts.SecretsDir, "openviking", "ovcli.conf")
		if err := writeReleaseCLIConfig(userConf, targetURL, userKey); err != nil {
			result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
			return result
		}
	}
	credentials.OpenViking.UserAPIKey = userKey
	result.Secrets = releaseSecrets(credentials, local)
	hermesHome, err := writeHermesTemplate(artifacts.SecretsDir, credentials.Model)
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	claudeSettings, claudeModel, err := writeClaudeAuthSettings(artifacts.SecretsDir)
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	if cfg.Options.ClaudeModel == "" {
		cfg.Options.ClaudeModel = claudeModel
	}
	commonEnv, err := releaseEnvironment(cfg.Options, artifacts, cfg.Prepared, cfg.Builds, cfg.Installed, targetURL, userConf, hermesHome, claudeSettings)
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	restoreCommon := applyEnvironment(commonEnv)
	defer restoreCommon()
	restoreInspection := applySanitizedHarnessEnvironment(nil)
	manifest, err := buildRunManifest(ctx, cfg.RunID, cfg.Label, cfg.CaseIDs, cfg.Prepared, cfg.Builds, cfg.Installed, cfg.EnvironmentManifestPath, cfg.Options, targetURL, userKey, local, credentials.Model)
	restoreInspection()
	if err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}
	if err := releasegate.WriteRunManifest(runManifestPath, manifest); err != nil {
		result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
		return result
	}

	preflightOpts := cfg.Options
	preflightOpts.CaseIDs = cfg.CaseIDs
	for _, item := range harnessPreflightCases(preflightOpts, artifacts.RuntimeDir) {
		restore := applySanitizedHarnessEnvironment(harnessEnvironment(item.Harness, credentials))
		res := runner.RunCaseContext(ctx, item.Case)
		restore()
		res.RunID, res.ArtifactPath = cfg.RunID, runManifestPath
		res.Provenance = map[string]any{"phase": "preflight", "harness": item.Harness, "attempt": cfg.Label}
		if res.Status != runner.StatusPass {
			res.Status = runner.StatusPreflightFail
		}
		runner.PrintResult(res)
		var failures []error
		if err := writeCaseEvidence(artifacts.EvidenceDir, res, releaseSecrets(credentials, local)); err != nil {
			failures = append(failures, fmt.Errorf("write %s preflight evidence: %w", item.Harness, err))
		}
		if err := appendReleaseRecord(recordsPath, res, releaseSecrets(credentials, local)); err != nil {
			failures = append(failures, fmt.Errorf("write %s preflight record: %w", item.Harness, err))
		}
		if res.Status != runner.StatusPass {
			failures = append(failures, fmt.Errorf("%s authentication/model preflight failed: %s", item.Harness, res.Detail))
		}
		if err := errors.Join(failures...); err != nil {
			result.Outcome.Primary, result.Class = err, releaseFailureEnvironment
			return result
		}
	}

	for _, id := range cfg.CaseIDs {
		restore := applySanitizedHarnessEnvironment(harnessEnvironment(caseHarness(id), credentials))
		res := runner.RunCaseContext(ctx, cases.All[id])
		restore()
		res.RunID, res.ArtifactPath = cfg.RunID, runManifestPath
		res.Provenance = map[string]any{"source_manifest": cfg.Prepared.ManifestPath, "target": cfg.Options.TargetMode, "attempt": cfg.Label}
		if len(res.CleanupClaims) > 0 {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			restoreCleanup := applySanitizedHarnessEnvironment(nil)
			cleanupErr := cleanupCaseClaims(cleanupCtx, cfg.Options.TargetMode, cfg.RunID, res.CleanupClaims, func(deleteCtx context.Context, uri string) error {
				return deleteReleaseURI(deleteCtx, ovBin, userConf, uri)
			})
			restoreCleanup()
			cancel()
			if cleanupErr != nil {
				res.CleanupStatus = runner.StatusCleanupFail
				res.CleanupDetail = cleanupErr.Error()
				result.Outcome.Cleanup = errors.Join(result.Outcome.Cleanup, fmt.Errorf("cleanup case %s: %w", id, cleanupErr))
				result.Class = releaseFailureEnvironment
			} else {
				res.CleanupStatus = runner.StatusPass
			}
		}
		runner.PrintResult(res)
		if err := writeCaseEvidence(artifacts.EvidenceDir, res, releaseSecrets(credentials, local)); err != nil {
			result.Outcome.Primary = errors.Join(result.Outcome.Primary, fmt.Errorf("write case %s evidence: %w", id, err))
			result.Class = releaseFailureEnvironment
		}
		if err := appendReleaseRecord(recordsPath, res, releaseSecrets(credentials, local)); err != nil {
			result.Outcome.Primary = errors.Join(result.Outcome.Primary, fmt.Errorf("write case %s record: %w", id, err))
			result.Class = releaseFailureEnvironment
		}
		if res.Status != runner.StatusPass {
			result.Outcome.Primary = errors.Join(result.Outcome.Primary, fmt.Errorf("case %s: %s: %s", id, res.Status, res.Detail))
			if releaseCaseFailureClass(res.Status) == releaseFailureEnvironment {
				result.Class = releaseFailureEnvironment
			} else {
				result.FailedCases = append(result.FailedCases, id)
				if result.Class == releaseFailureNone {
					result.Class = releaseFailureCase
				}
			}
		}
		if ctx.Err() != nil {
			result.Outcome.Primary = errors.Join(result.Outcome.Primary, ctx.Err())
			result.Class = releaseFailureEnvironment
			return result
		}
		if local != nil {
			if err := local.Service.UnexpectedExit(); err != nil {
				result.Outcome.Primary = errors.Join(result.Outcome.Primary, err)
				result.Class = releaseFailureEnvironment
				return result
			}
		}
	}
	return result
}

func releaseCaseFailureClass(status string) releaseFailureClass {
	switch status {
	case runner.StatusJudgeFail, runner.StatusExecutionFail, runner.StatusTimedOut:
		return releaseFailureEnvironment
	default:
		return releaseFailureCase
	}
}

func cleanupReleaseURIs(ctx context.Context, mode releasegate.TargetMode, ownership *releasegate.OwnershipRegistry, primary error, deleteURI func(context.Context, string) error) releasegate.Outcome {
	_ = mode
	return ownership.Cleanup(ctx, primary, deleteURI)
}

func cleanupCaseClaims(ctx context.Context, mode releasegate.TargetMode, runID string, claims []ops.CleanupClaim, deleteURI func(context.Context, string) error) error {
	ownership := releasegate.NewOwnershipRegistry(runID)
	for _, claim := range claims {
		if err := ownership.Claim(claim); err != nil {
			return err
		}
	}
	return cleanupReleaseURIs(ctx, mode, ownership, nil, deleteURI).Err()
}

func loadReleaseCredentials(path string) (releasegate.RuntimeCredentials, error) {
	if strings.TrimSpace(path) == "" {
		return releasegate.RuntimeCredentials{}, fmt.Errorf("--env-file is required for local OpenViking, OpenClaw, and Hermes credentials")
	}
	credentials, err := releasegate.LoadEnvFileWithOverrides(path, os.Environ())
	if err != nil {
		return releasegate.RuntimeCredentials{}, fmt.Errorf("load --env-file: %w", err)
	}
	return credentials, nil
}

func requireLocalProvider(model releasegate.ModelCredentials) error {
	missing := []string{}
	for name, value := range map[string]string{
		"LLM API key": model.LLMAPIKey, "LLM base URL": model.LLMBaseURL, "LLM model": model.LLMModel,
		"embedding API key": model.EmbeddingAPIKey, "embedding base URL": model.EmbeddingBaseURL, "embedding model": model.EmbeddingModel,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("local OpenViking provider configuration is incomplete: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func newReleaseRunID(now time.Time) (string, error) {
	random := make([]byte, 5)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "run-" + now.UTC().Format("20060102t150405z") + "-" + hex.EncodeToString(random), nil
}

func needsHarness(ids []string, prefix string) bool {
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func requiredHarnessPackages(ids []string) []string {
	set := map[string]bool{}
	for _, id := range ids {
		switch caseHarness(id) {
		case "claude":
			set["claude-code"] = true
		case "hermes":
			set["hermes-agent"] = true
		case "codex", "openclaw", "opencode", "pi":
			set[caseHarness(id)] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func writeReleaseCLIConfig(path, baseURL, apiKey string) error {
	if baseURL == "" || apiKey == "" {
		return fmt.Errorf("OpenViking URL and user API key are required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]any{
		"url": baseURL, "api_key": apiKey, "output": "json", "echo_command": false,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func writeHermesTemplate(secretRoot string, model releasegate.ModelCredentials) (string, error) {
	home := filepath.Join(secretRoot, "hermes-template")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	apiMode := "chat_completions"
	if model.LLMProtocol == releasegate.ProtocolOpenAIResponses {
		apiMode = "codex_responses"
	}
	config := map[string]any{
		"model": map[string]any{"provider": "custom:ovtest", "default": model.LLMModel},
		"agent": map[string]any{"reasoning_effort": "none"},
		// An explicit empty CLI selection disables Hermes' default-on plugin and
		// context-engine toolsets. Tool E2Es opt back into `memory` explicitly.
		"platform_toolsets":     map[string]any{"cli": []any{}},
		"known_plugin_toolsets": map[string]any{"cli": []any{"memory"}},
		"custom_providers": []any{map[string]any{
			"name": "ovtest", "base_url": model.LLMBaseURL, "model": model.LLMModel,
			"api_mode": apiMode, "key_env": "OV_TEST_HERMES_LLM_API_KEY",
		}},
		"memory": map[string]any{"provider": "openviking"},
	}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), append(raw, '\n'), 0o600); err != nil {
		return "", err
	}
	return home, nil
}

func writeClaudeAuthSettings(secretRoot string) (path, model string, err error) {
	source := strings.TrimSpace(os.Getenv("OV_TEST_CLAUDE_AUTH_SETTINGS"))
	if source == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", nil
		}
		source = filepath.Join(home, ".claude", "settings.json")
	}
	raw, readErr := os.ReadFile(source)
	if os.IsNotExist(readErr) && os.Getenv("OV_TEST_CLAUDE_AUTH_SETTINGS") == "" {
		return "", "", nil
	}
	if readErr != nil {
		return "", "", fmt.Errorf("read Claude machine auth settings: %w", readErr)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", "", fmt.Errorf("decode Claude machine auth settings: %w", err)
	}
	sourceEnv, _ := settings["env"].(map[string]any)
	allowed := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_BASE_URL": true,
		"ANTHROPIC_MODEL": true, "ANTHROPIC_SMALL_FAST_MODEL": true,
		"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true,
		"AWS_REGION": true, "AWS_PROFILE": true, "CLOUD_ML_REGION": true, "ANTHROPIC_VERTEX_PROJECT_ID": true,
	}
	isolatedEnv := map[string]string{}
	for key, value := range sourceEnv {
		if allowed[key] {
			isolatedEnv[key] = fmt.Sprint(value)
		}
	}
	if len(isolatedEnv) == 0 {
		return "", "", nil
	}
	model = isolatedEnv["ANTHROPIC_MODEL"]
	destination := filepath.Join(secretRoot, "claude-auth-settings.json")
	body, err := json.MarshalIndent(map[string]any{"env": isolatedEnv}, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(destination, append(body, '\n'), 0o600); err != nil {
		return "", "", err
	}
	return destination, model, nil
}

func releaseEnvironment(opts releaseOptions, artifacts *releasegate.RunArtifacts, prepared *releasegate.PreparedSources, builds map[string]releasegate.BuildProvenance, installed *releasegate.PreparedHarnesses, targetURL, userConf, hermesHome, claudeSettings string) (map[string]string, error) {
	ovBin, err := releasegate.BuiltArtifact(builds, "OpenViking", "ov")
	if err != nil {
		return nil, err
	}
	if installed == nil {
		return nil, fmt.Errorf("official harness installations are unavailable")
	}
	openVikingRepo := prepared.Worktrees["OpenViking"]
	env := map[string]string{
		"OV_TEST_RUN_ID":                     runIDFromArtifacts(artifacts),
		"OV_TEST_STATE_DIR":                  filepath.Join(artifacts.RuntimeDir, "harnesses"),
		"OV_TEST_SECRET_STATE_DIR":           filepath.Join(artifacts.SecretsDir, "harnesses"),
		"OV_TEST_CONF_DIR":                   filepath.Dir(userConf),
		"OV_TEST_OV_BIN":                     ovBin,
		"OV_TEST_OPENVIKING_REPO":            openVikingRepo,
		"OV_TEST_OPENVIKING_CONF":            filepath.Join(artifacts.SecretsDir, "openviking", "ov.conf"),
		"OV_TEST_HERMES_HOME_TEMPLATE":       hermesHome,
		"OV_TEST_PI_EXTENSION_ROOT":          filepath.Join(openVikingRepo, "examples", "pi-coding-agent-extension"),
		"OV_TEST_CODEX_OPENVIKING_URL":       targetURL,
		"OV_TEST_CLAUDE_OPENVIKING_URL":      targetURL,
		"OV_TEST_OPENCODE_OPENVIKING_URL":    targetURL,
		"OV_TEST_PI_OPENVIKING_URL":          targetURL,
		"OV_TEST_OPENCLAW_OPENVIKING_URL":    targetURL,
		"OV_TEST_HERMES_OPENVIKING_ENDPOINT": targetURL,
		"OV_TEST_CLEANUP_MODE":               "none",
		"OPENVIKING_QUEUE_SCOPE_KEY_FILE":    filepath.Join(artifacts.SecretsDir, "openviking", "queue-scope.key"),
	}
	for harness, key := range map[string]string{
		"codex": "OV_TEST_CODEX_BIN", "claude-code": "OV_TEST_CLAUDE_BIN", "openclaw": "OV_TEST_OPENCLAW_BIN",
		"opencode": "OV_TEST_OPENCODE_BIN", "pi": "OV_TEST_PI_BIN", "hermes-agent": "OV_TEST_HERMES_BIN",
	} {
		if binary := installed.Binaries[harness]; binary != "" {
			env[key] = binary
			if harness == "hermes-agent" {
				env["OV_TEST_HERMES_PYTHON"] = filepath.Join(filepath.Dir(binary), "python")
			}
		}
	}
	if opts.CodexModel != "" {
		env["OV_TEST_CODEX_MODEL"] = opts.CodexModel
	}
	if opts.ClaudeModel != "" {
		env["OV_TEST_CLAUDE_MODEL"] = opts.ClaudeModel
	}
	if claudeSettings != "" {
		env["OV_TEST_CLAUDE_SETTINGS"] = claudeSettings
	}
	if opts.OpenCodeModel != "" {
		env["OV_TEST_OPENCODE_MODEL"] = opts.OpenCodeModel
	}
	if template := defaultOpenCodeConfigTemplate(); template != "" {
		env["OV_TEST_OPENCODE_CONFIG_TEMPLATE"] = template
	}
	if localPath := filepath.Join(artifacts.SecretsDir, "openviking", "ov.conf"); fileExists(localPath) {
		env["OV_TEST_OPENVIKING_CONF"] = localPath
	} else {
		delete(env, "OV_TEST_OPENVIKING_CONF")
	}
	return env, nil
}

func defaultOpenCodeConfigTemplate() string {
	if explicit := strings.TrimSpace(os.Getenv("OV_TEST_OPENCODE_CONFIG_TEMPLATE")); explicit != "" {
		return explicit
	}
	var roots []string
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		roots = append(roots, xdg)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".config"))
	}
	for _, rootDir := range roots {
		for _, name := range []string{"opencode.json", "opencode.jsonc"} {
			candidate := filepath.Join(rootDir, "opencode", name)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate
			}
		}
	}
	return ""
}

func runIDFromArtifacts(artifacts *releasegate.RunArtifacts) string {
	return filepath.Base(artifacts.RunDir)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func harnessEnvironment(harness string, credentials releasegate.RuntimeCredentials) map[string]string {
	switch harness {
	case "openclaw":
		return credentials.OpenClawProcess()
	case "hermes":
		return credentials.HermesProcess()
	case "codex":
		return credentials.CodexProcess()
	case "claude":
		return credentials.ClaudeProcess()
	case "opencode":
		return credentials.OpenCodeProcess()
	case "pi":
		return credentials.PiProcess()
	case "openviking":
		out := credentials.OpenVikingCaseProcess()
		out["OVTEST_JUDGE_TIMEOUT"] = "180"
		out["OVTEST_JUDGE_RETRIES"] = "1"
		for _, key := range []string{"OVTEST_JUDGE_MODEL", "OVTEST_JUDGE_TIMEOUT", "OVTEST_JUDGE_RETRIES"} {
			if value := strings.TrimSpace(os.Getenv(key)); value != "" {
				out[key] = value
			}
		}
		return out
	default:
		return map[string]string{}
	}
}

func caseHarness(id string) string {
	switch {
	case strings.HasPrefix(id, "openclaw-"):
		return "openclaw"
	case strings.HasPrefix(id, "hermes-"):
		return "hermes"
	case strings.HasPrefix(id, "codex-"):
		return "codex"
	case strings.HasPrefix(id, "claude-"):
		return "claude"
	case strings.HasPrefix(id, "opencode-"):
		return "opencode"
	case strings.HasPrefix(id, "pi-"):
		return "pi"
	default:
		return "openviking"
	}
}

func applyEnvironment(values map[string]string) func() {
	type oldValue struct {
		value string
		set   bool
	}
	old := make(map[string]oldValue, len(values))
	for key, value := range values {
		previous, ok := os.LookupEnv(key)
		old[key] = oldValue{value: previous, set: ok}
		_ = os.Setenv(key, value)
	}
	return func() {
		for key, previous := range old {
			if previous.set {
				_ = os.Setenv(key, previous.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
}

// applySanitizedHarnessEnvironment removes ambient provider credentials before
// adding the typed, harness-specific values for one subprocess lane. The
// returned closure restores the operator environment exactly.
func applySanitizedHarnessEnvironment(values map[string]string) func() {
	current := map[string]string{}
	for _, item := range os.Environ() {
		if key, value, ok := strings.Cut(item, "="); ok {
			current[key] = value
		}
	}
	clean := releasegate.ScrubProviderEnv(current)
	touched := map[string]struct{}{}
	for key := range current {
		if _, keep := clean[key]; !keep {
			touched[key] = struct{}{}
			_ = os.Unsetenv(key)
		}
	}
	for key, value := range values {
		touched[key] = struct{}{}
		_ = os.Setenv(key, value)
	}
	return func() {
		for key := range touched {
			if value, ok := current[key]; ok {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
}

type harnessPreflight struct {
	Harness string
	Case    runner.Case
}

func harnessPreflightCases(opts releaseOptions, runtimeRoot string) []harnessPreflight {
	const token = "OVTEST_PREFLIGHT_OK"
	prompt := "Reply with exactly " + token + ". Do not use tools."
	root := filepath.Join(runtimeRoot, "preflight")
	runID := preflightSessionSuffix(runtimeRoot)
	all := []harnessPreflight{
		{Harness: "openclaw", Case: preflightCase("preflight-openclaw", openclawadapter.Chat, dag.Cfg{
			"message": prompt, "session_id": "ovtest-preflight-" + runID, "state_dir": filepath.Join(root, "openclaw"),
			"auto_capture": false, "auto_recall": false, "timeout": 240,
		}, "reply", token)},
		{Harness: "hermes", Case: preflightCase("preflight-hermes", hermesadapter.Chat, dag.Cfg{
			"message": prompt, "home": filepath.Join(root, "hermes"), "timeout": 240,
		}, "reply", token)},
		{Harness: "codex", Case: preflightCase("preflight-codex", codexadapter.Exec, dag.Cfg{
			"message": prompt, "cwd": filepath.Join(root, "codex", "cwd"), "state_dir": filepath.Join(root, "codex", "state"),
			"auto_capture": false, "auto_recall": false, "sandbox": "read-only", "timeout": 240,
		}, "reply", token)},
		{Harness: "claude", Case: preflightCase("preflight-claude", claudeadapter.Exec, dag.Cfg{
			"message": prompt, "cwd": filepath.Join(root, "claude", "cwd"), "state_dir": filepath.Join(root, "claude", "state"),
			"auto_capture": false, "auto_recall": false, "setting_sources": "", "timeout": 240,
		}, "reply", token)},
		{Harness: "opencode", Case: preflightCase("preflight-opencode", opencodeadapter.Exec, dag.Cfg{
			"message": prompt, "project_dir": filepath.Join(root, "opencode", "project"), "state_dir": filepath.Join(root, "opencode", "state"),
			"auto_capture": false, "auto_recall": false, "timeout": 300, "empty_reply_retries": 1,
		}, "reply", token)},
		{Harness: "pi", Case: preflightCase("preflight-pi", piadapter.Exec, dag.Cfg{
			"message": prompt, "project_dir": filepath.Join(root, "pi", "project"), "state_dir": filepath.Join(root, "pi", "state"),
			"auto_capture": false, "takeover": false, "timeout": 300,
		}, "reply", token)},
	}
	required := map[string]bool{}
	for _, id := range opts.CaseIDs {
		if harness := caseHarness(id); harness != "openviking" {
			required[harness] = true
		}
	}
	selected := make([]harnessPreflight, 0, len(required))
	for _, item := range all {
		if required[item.Harness] {
			selected = append(selected, item)
		}
	}
	return selected
}

func preflightSessionSuffix(runtimeRoot string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(runtimeRoot)))
	return hex.EncodeToString(digest[:8])
}

func preflightCase(id string, factory dag.Factory, config dag.Cfg, output, token string) runner.Case {
	return runner.Case{ID: id, Goal: "Validate executable, authentication, and model reachability before product regression cases.", Reference: token,
		Build: func(b *dag.Builder) {
			user := runner.ConfiguredUser(b, "user")
			run := b.Add(factory, dag.Spec{Name: "invoke", In: dag.In{"user_key": user}, Config: config})
			b.Add(checks.Text, dag.Spec{Name: "preflight_check", In: dag.In{"text": run.Out(output), "after": run}, Config: dag.Cfg{"expect": []string{token}}})
		},
	}
}

func buildEnvironmentManifest(ctx context.Context, runID string, opts releaseOptions, prepared, baseline *releasegate.PreparedSources, installed *releasegate.PreparedHarnesses, model releasegate.ModelCredentials) (releasegate.EnvironmentManifest, error) {
	toolchain, err := releasegate.CaptureToolchain(ctx)
	if err != nil {
		return releasegate.EnvironmentManifest{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return releasegate.EnvironmentManifest{}, err
	}
	ovtestHash, err := releasegate.FileSHA256(executable)
	if err != nil {
		return releasegate.EnvironmentManifest{}, err
	}
	caseIDs := append([]string(nil), opts.CaseIDs...)
	sort.Strings(caseIDs)
	definition, err := json.Marshal(struct {
		Suite string   `json:"suite"`
		Cases []string `json:"cases"`
	}{Suite: opts.Suite, Cases: caseIDs})
	if err != nil {
		return releasegate.EnvironmentManifest{}, err
	}
	definitionHash := sha256.Sum256(definition)
	plugins := map[string]releasegate.PluginProvenance{}
	for name, relative := range selectedOpenVikingPlugins(caseIDs) {
		path := filepath.Join(prepared.Worktrees["OpenViking"], "examples", relative)
		hash, hashErr := releasegate.HashTree(path)
		if hashErr != nil {
			return releasegate.EnvironmentManifest{}, fmt.Errorf("hash %s plugin: %w", name, hashErr)
		}
		plugins[name] = releasegate.PluginProvenance{Path: filepath.ToSlash(filepath.Join("examples", relative)), TreeSHA256: hash}
	}
	manifest := releasegate.EnvironmentManifest{
		EnvironmentID: runID, Mode: opts.SourceMode, Source: prepared.Manifest, Harnesses: installed.Manifest,
		Platform: releasegate.NewPlatformIdentity(toolchain),
		Gate:     releasegate.GateIdentity{Suite: opts.Suite, Cases: caseIDs, Definition: hex.EncodeToString(definitionHash[:]), OvtestBinary: ovtestHash},
		Trust:    releasegate.TrustDecision{CredentialedExecution: true, Authority: "operator --trusted-execution"}, Plugins: plugins,
		Configuration: map[string]string{
			"qualification_attempts": fmt.Sprint(opts.QualificationRuns),
			"openviking_llm_model":   model.LLMModel, "openviking_embedding_model": model.EmbeddingModel,
			"llm_protocol": model.LLMProtocol, "llm_endpoint_sha256": stableValueHash(model.LLMBaseURL),
			"embedding_endpoint_sha256": stableValueHash(model.EmbeddingBaseURL),
			"codex_model":               firstNonEmpty(opts.CodexModel, "cli-default"), "claude_model": firstNonEmpty(opts.ClaudeModel, "cli-default"),
			"opencode_model": firstNonEmpty(opts.OpenCodeModel, "cli-default"), "target_mode": string(opts.TargetMode),
		},
	}
	if template := defaultOpenCodeConfigTemplate(); template != "" {
		hash, hashErr := releasegate.FileSHA256(template)
		if hashErr != nil {
			return releasegate.EnvironmentManifest{}, fmt.Errorf("hash OpenCode configuration template: %w", hashErr)
		}
		manifest.Configuration["opencode_template_sha256"] = hash
	}
	if opts.OpenVikingConfig != "" {
		hash, hashErr := releasegate.FileSHA256(opts.OpenVikingConfig)
		if hashErr != nil {
			return releasegate.EnvironmentManifest{}, fmt.Errorf("hash OpenViking configuration template: %w", hashErr)
		}
		manifest.Configuration["openviking_template_sha256"] = hash
	}
	if baseline != nil {
		base := baseline.Manifest
		manifest.Baseline = &base
	}
	if opts.SourceMode == releasegate.SourceManifest {
		manifest.ReplayOf = opts.SourceManifest
	}
	return manifest, nil
}

func stableValueHash(value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(digest[:])
}

func buildRunManifest(ctx context.Context, runID, attempt string, caseIDs []string, prepared *releasegate.PreparedSources, builds map[string]releasegate.BuildProvenance, installed *releasegate.PreparedHarnesses, environmentManifestPath string, opts releaseOptions, targetURL, userKey string, local *releasegate.LocalOpenViking, model releasegate.ModelCredentials) (releasegate.RunManifest, error) {
	version, err := targetVersion(ctx, targetURL, userKey)
	if err != nil {
		return releasegate.RunManifest{}, err
	}
	executables := map[string]releasegate.ExecutableProvenance{}
	versionArgs := map[string][]string{
		"ov": {"version"}, "hermes": {"--version"},
		"openclaw": {"--version"}, "opencode": {"--version"}, "codex": {"--version"}, "claude": {"--version"}, "pi": {"--version"},
	}
	paths := map[string]string{
		"openviking-server": builds["OpenViking"].Artifacts["server"].Path,
		"ov":                builds["OpenViking"].Artifacts["ov"].Path,
	}
	for harness, binary := range installed.Binaries {
		name := harness
		if harness == "hermes-agent" {
			name = "hermes"
		} else if harness == "claude-code" {
			name = "claude"
		}
		paths[name] = binary
	}
	for name, binary := range paths {
		var item releasegate.ExecutableProvenance
		var err error
		if name == "openviking-server" {
			item, err = releasegate.InspectExecutableArtifact(binary, version)
		} else {
			item, err = releasegate.InspectExecutable(ctx, binary, versionArgs[name]...)
		}
		if err != nil {
			return releasegate.RunManifest{}, fmt.Errorf("inspect %s executable: %w", name, err)
		}
		if rel, relErr := filepath.Rel(filepath.Dir(environmentManifestPath), item.Path); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			item.Artifact = filepath.ToSlash(rel)
		} else if name == "openviking-server" || name == "ov" {
			item.Artifact = "source:OpenViking/" + filepath.ToSlash(strings.TrimPrefix(item.Path, prepared.Worktrees["OpenViking"]+string(filepath.Separator)))
		}
		executables[name] = item
	}
	plugins := map[string]releasegate.PluginProvenance{}
	for name, relative := range selectedOpenVikingPlugins(caseIDs) {
		path := filepath.Join(prepared.Worktrees["OpenViking"], "examples", relative)
		hash, err := releasegate.HashTree(path)
		if err != nil {
			return releasegate.RunManifest{}, fmt.Errorf("hash %s plugin: %w", name, err)
		}
		relative, _ := filepath.Rel(prepared.Worktrees["OpenViking"], path)
		plugins[name] = releasegate.PluginProvenance{Path: filepath.ToSlash(relative), TreeSHA256: hash}
	}
	target := releasegate.TargetProvenance{Mode: opts.TargetMode, URL: targetURL, Version: version, AuthMode: "api_key", Canonical: opts.TargetMode == releasegate.TargetLocal}
	if !target.Canonical {
		target.DiagnosticReason = "external health does not expose an exact OpenViking source identity matching the candidate"
	}
	if local != nil {
		target.ConfigHash, err = releasegate.FileSHA256(local.ConfigPath)
		if err != nil {
			return releasegate.RunManifest{}, err
		}
	}
	return releasegate.RunManifest{
		RunID: runID, Attempt: attempt, Suite: opts.Suite, Cases: append([]string(nil), caseIDs...), SourceManifestPath: prepared.ManifestPath, EnvironmentManifestPath: environmentManifestPath, Target: target,
		Executables: executables, Plugins: plugins, Builds: builds,
		Models: map[string]string{
			"openviking_llm": model.LLMModel, "openviking_embedding": model.EmbeddingModel,
			"openclaw": model.LLMModel, "hermes": model.LLMModel,
			"codex":    firstNonEmpty(opts.CodexModel, "cli-default"),
			"claude":   firstNonEmpty(opts.ClaudeModel, "cli-default"),
			"opencode": firstNonEmpty(opts.OpenCodeModel, "cli-default"),
			"pi":       model.LLMModel,
		},
	}, nil
}

func selectedOpenVikingPlugins(caseIDs []string) map[string]string {
	available := map[string]string{
		"claude": "claude-code-memory-plugin", "codex": "codex-memory-plugin", "openclaw": "openclaw-plugin",
		"opencode": "opencode-plugin", "pi": "pi-coding-agent-extension",
	}
	selected := map[string]string{}
	for _, id := range caseIDs {
		harness := caseHarness(id)
		if path, ok := available[harness]; ok {
			selected[harness] = path
		}
	}
	return selected
}

func targetVersion(ctx context.Context, baseURL, apiKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/health", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OpenViking health provenance returned HTTP %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return healthVersion(body), nil
}

func healthVersion(body map[string]any) string {
	for _, key := range []string{"version", "service_version"} {
		if value, ok := body[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func deleteReleaseURI(ctx context.Context, ovBin, confPath, uri string) error {
	env := map[string]string{"OPENVIKING_CLI_CONFIG_FILE": confPath}
	run := func(recursive bool) cli.Result {
		args := []string{"rm", uri}
		if recursive {
			args = append(args, "--recursive")
		}
		args = append(args, "--wait", "--timeout", "180")
		return cli.RunContext(ctx, append([]string{ovBin}, args...), env, 0, 240)
	}
	recursive := cleanupURIRequiresRecursive(uri)
	result := run(recursive)
	detail := result.Stdout + "\n" + result.Stderr
	if result.ExitCode == 0 || cleanupNotFound(detail) {
		return nil
	}
	if !recursive && cleanupNeedsRecursive(detail) {
		result = run(true)
		if result.ExitCode == 0 || cleanupNotFound(result.Stdout+"\n"+result.Stderr) {
			return nil
		}
	}
	return fmt.Errorf("exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
}

func cleanupURIRequiresRecursive(rawURI string) bool {
	parsed, err := url.Parse(rawURI)
	if err != nil || parsed.Scheme != "viking" || parsed.Host != "user" {
		return false
	}
	segments := strings.FieldsFunc(parsed.Path, func(r rune) bool { return r == '/' })
	return len(segments) >= 3 && segments[1] == "sessions"
}

func cleanupNeedsRecursive(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "directory not empty") ||
		strings.Contains(text, "is a directory") ||
		strings.Contains(text, "directory without --recursive") ||
		strings.Contains(text, "use --recursive")
}

func cleanupNotFound(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "not_found") || strings.Contains(text, "not found") || strings.Contains(text, "404")
}

func releaseSecrets(credentials releasegate.RuntimeCredentials, local *releasegate.LocalOpenViking) []string {
	values := []string{credentials.Model.LLMAPIKey, credentials.Model.EmbeddingAPIKey, credentials.HarnessLLMAPIKey, credentials.OpenViking.UserAPIKey}
	if local != nil {
		values = append(values, local.RootAPIKey, local.UserAPIKey)
	}
	return values
}

func writeCaseEvidence(dir string, res runner.Result, secrets []string) error {
	raw, err := marshalRedacted(res, secrets, true)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, safeFileName(res.ID)+".json")
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func appendReleaseRecord(path string, res runner.Result, secrets []string) error {
	raw, err := marshalRedacted(runner.ResultRecord(res), secrets, false)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func marshalRedacted(value any, secrets []string, indent bool) ([]byte, error) {
	var raw []byte
	var err error
	if indent {
		raw, err = json.MarshalIndent(value, "", "  ")
	} else {
		raw, err = json.Marshal(value)
	}
	if err != nil {
		return nil, err
	}
	return releasegate.RedactBytes(raw, secrets), nil
}

func safeFileName(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func writeReleaseSummary(path, runID string, outcome releasegate.Outcome, classification releaseClassification, secrets []string) error {
	status := "pass"
	if outcome.Primary != nil {
		status = "primary_failed"
	}
	if outcome.Cleanup != nil {
		if outcome.Primary != nil {
			status = "primary_and_cleanup_failed"
		} else {
			status = "cleanup_failed"
		}
	}
	raw, err := json.MarshalIndent(map[string]any{
		"schema_version": 1, "run_id": runID, "status": status,
		"classification": classification,
		"primary_error":  errorString(outcome.Primary), "cleanup_error": errorString(outcome.Cleanup),
		"finished_at": time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(releasegate.RedactBytes(raw, secrets), '\n'), 0o600)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
