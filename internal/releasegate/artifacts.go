package releasegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type RunArtifacts struct {
	RunDir      string
	RuntimeDir  string
	EvidenceDir string
	SecretsDir  string
}

func NewRunArtifacts(root, runID string) (*RunArtifacts, error) {
	if root == "" || !safeName.MatchString(runID) {
		return nil, fmt.Errorf("artifacts: root and safe run ID are required")
	}
	runDir := filepath.Join(root, "runs", runID)
	a := &RunArtifacts{
		RunDir: runDir, RuntimeDir: filepath.Join(runDir, "runtime"), EvidenceDir: filepath.Join(runDir, "evidence"),
		SecretsDir: filepath.Join(runDir, "secrets"),
	}
	for _, dir := range []string{a.RunDir, a.RuntimeDir, a.EvidenceDir, a.SecretsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func NewAttemptArtifacts(run *RunArtifacts, attemptID string) (*RunArtifacts, error) {
	if run == nil || !safeName.MatchString(attemptID) {
		return nil, fmt.Errorf("attempt artifacts: run and safe attempt ID are required")
	}
	a := &RunArtifacts{
		RunDir: run.RunDir, RuntimeDir: filepath.Join(run.RuntimeDir, attemptID),
		EvidenceDir: filepath.Join(run.EvidenceDir, attemptID), SecretsDir: filepath.Join(run.SecretsDir, attemptID),
	}
	for _, dir := range []string{a.RuntimeDir, a.EvidenceDir, a.SecretsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Finalize always removes the dedicated secret directory. Successful runs
// discard bulky runtime state and, unless explicitly requested, full evidence;
// immutable manifests, results, and the run summary remain in RunDir.
// Failures retain redacted evidence and runtime for diagnosis.
func (a *RunArtifacts) Finalize(success, keepSuccessEvidence bool) error {
	if a == nil {
		return nil
	}
	var errs []error
	if err := os.RemoveAll(a.SecretsDir); err != nil {
		errs = append(errs, err)
	}
	if success {
		if err := os.RemoveAll(a.RuntimeDir); err != nil {
			errs = append(errs, err)
		}
		if !keepSuccessEvidence {
			if err := os.RemoveAll(a.EvidenceDir); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func PruneExpiredRuns(ctx context.Context, root string, retention time.Duration, now time.Time) ([]string, error) {
	if retention <= 0 {
		return nil, fmt.Errorf("artifact retention must be positive")
	}
	unlock, err := lockSourceCache(ctx, root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !safeName.MatchString(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			errs = append(errs, infoErr)
			continue
		}
		if now.Sub(info.ModTime()) < retention {
			continue
		}
		runDir := filepath.Join(root, "runs", entry.Name())
		manifest, manifestErr := ReadManifest(filepath.Join(runDir, "source-manifest.json"))
		if manifestErr != nil {
			if err := pruneIncompleteRun(ctx, root, entry.Name(), runDir); err != nil {
				errs = append(errs, fmt.Errorf("prune incomplete %s: %w", entry.Name(), err))
				continue
			}
			removed = append(removed, entry.Name())
			continue
		}
		failed := false
		for _, source := range manifest.Sources {
			mirror := filepath.Join(root, "mirrors", source.Name+".git")
			ref := "refs/ovtest/runs/" + entry.Name() + "/" + source.Name
			if _, refErr := gitOutput(ctx, mirror, "update-ref", "-d", ref); refErr != nil {
				errs = append(errs, fmt.Errorf("prune %s: %w", entry.Name(), refErr))
				failed = true
			}
		}
		if failed {
			continue
		}
		if err := os.RemoveAll(runDir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, entry.Name())
	}
	sort.Strings(removed)
	return removed, errors.Join(errs...)
}

func pruneIncompleteRun(ctx context.Context, root, runID, runDir string) error {
	mirrors, err := os.ReadDir(filepath.Join(root, "mirrors"))
	if errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(runDir)
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range mirrors {
		if !entry.IsDir() || filepath.Ext(entry.Name()) != ".git" {
			continue
		}
		mirror := filepath.Join(root, "mirrors", entry.Name())
		worktrees, listErr := gitOutput(ctx, mirror, "worktree", "list", "--porcelain")
		if listErr != nil {
			errs = append(errs, listErr)
			continue
		}
		for _, line := range strings.Split(worktrees, "\n") {
			path := strings.TrimPrefix(line, "worktree ")
			if path == line || !pathWithin(runDir, path) {
				continue
			}
			if _, removeErr := gitOutput(ctx, mirror, "worktree", "remove", "--force", path); removeErr != nil {
				errs = append(errs, removeErr)
			}
		}
		refs, refsErr := gitOutput(ctx, mirror, "for-each-ref", "--format=%(refname)", "refs/ovtest/runs/"+runID+"/")
		if refsErr != nil {
			errs = append(errs, refsErr)
			continue
		}
		for _, ref := range strings.Fields(refs) {
			if _, deleteErr := gitOutput(ctx, mirror, "update-ref", "-d", ref); deleteErr != nil {
				errs = append(errs, deleteErr)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return os.RemoveAll(runDir)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
