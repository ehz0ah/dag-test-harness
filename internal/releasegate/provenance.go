package releasegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const RunManifestSchemaVersion = 1

type ExecutableProvenance struct {
	Path     string `json:"path"`
	Artifact string `json:"artifact,omitempty"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
}

type PluginProvenance struct {
	Path       string `json:"path"`
	TreeSHA256 string `json:"tree_sha256"`
}

type TargetProvenance struct {
	Mode             TargetMode `json:"mode"`
	URL              string     `json:"url"`
	Version          string     `json:"version,omitempty"`
	AuthMode         string     `json:"auth_mode"`
	ConfigHash       string     `json:"config_sha256,omitempty"`
	Canonical        bool       `json:"canonical"`
	DiagnosticReason string     `json:"diagnostic_reason,omitempty"`
}

type RunManifest struct {
	SchemaVersion             int                             `json:"schema_version"`
	RunID                     string                          `json:"run_id"`
	Attempt                   string                          `json:"attempt"`
	Suite                     string                          `json:"suite"`
	Cases                     []string                        `json:"cases"`
	CreatedAt                 time.Time                       `json:"created_at"`
	SourceManifestPath        string                          `json:"source_manifest"`
	SourceManifestSHA256      string                          `json:"source_manifest_sha256"`
	EnvironmentManifestPath   string                          `json:"environment_manifest"`
	EnvironmentManifestSHA256 string                          `json:"environment_manifest_sha256"`
	Target                    TargetProvenance                `json:"openviking_target"`
	Executables               map[string]ExecutableProvenance `json:"executables,omitempty"`
	Plugins                   map[string]PluginProvenance     `json:"plugins,omitempty"`
	Builds                    map[string]BuildProvenance      `json:"builds,omitempty"`
	Models                    map[string]string               `json:"models,omitempty"`
}

func InspectExecutable(ctx context.Context, binary string, versionArgs ...string) (ExecutableProvenance, error) {
	versionCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path, err := executablePath(binary)
	if err != nil {
		return ExecutableProvenance{}, err
	}
	args := append([]string{path}, versionArgs...)
	version, err := command(versionCtx, args...)
	if err != nil {
		return ExecutableProvenance{}, err
	}
	return InspectExecutableArtifact(path, version)
}

// InspectExecutableArtifact records an executable without invoking it. This is
// used when a service exposes its version through an already-isolated health
// endpoint and its CLI has no guaranteed side-effect-free version command.
func InspectExecutableArtifact(binary, version string) (ExecutableProvenance, error) {
	path, err := executablePath(binary)
	if err != nil {
		return ExecutableProvenance{}, err
	}
	hash, err := FileSHA256(path)
	if err != nil {
		return ExecutableProvenance{}, err
	}
	return ExecutableProvenance{Path: path, Version: strings.TrimSpace(version), SHA256: hash}, nil
}

func executablePath(binary string) (string, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func HashTree(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
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
		_, _ = io.WriteString(hash, filepath.ToSlash(rel))
		_, _ = io.WriteString(hash, "\x00")
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
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(hash, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case entry.Type()&os.ModeSymlink != 0:
			_, _ = io.WriteString(hash, "symlink\x00")
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(hash, target)
		case entry.IsDir():
			_, _ = io.WriteString(hash, "dir\x00")
		default:
			return fmt.Errorf("hash tree: unsupported file type at %s", rel)
		}
		_, _ = io.WriteString(hash, "\x00")
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func WriteRunManifest(path string, manifest RunManifest) error {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = RunManifestSchemaVersion
	}
	if manifest.SchemaVersion != RunManifestSchemaVersion || !safeName.MatchString(manifest.RunID) || !safeName.MatchString(manifest.Attempt) {
		return fmt.Errorf("run manifest: unsupported schema or unsafe run ID")
	}
	if manifest.Suite == "" || len(manifest.Cases) == 0 {
		return fmt.Errorf("run manifest: suite and cases are required")
	}
	if manifest.SourceManifestPath == "" {
		return fmt.Errorf("run manifest: source manifest path is required")
	}
	sourceHash, err := FileSHA256(manifest.SourceManifestPath)
	if err != nil {
		return err
	}
	manifest.SourceManifestSHA256 = sourceHash
	if manifest.EnvironmentManifestPath == "" {
		return fmt.Errorf("run manifest: environment manifest path is required")
	}
	environmentHash, err := FileSHA256(manifest.EnvironmentManifestPath)
	if err != nil {
		return err
	}
	manifest.EnvironmentManifestSHA256 = environmentHash
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".run-manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("run manifest already exists: %s", path)
		}
		return err
	}
	return nil
}

func ReadRunManifest(path string) (RunManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RunManifest{}, err
	}
	var manifest RunManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return RunManifest{}, err
	}
	if manifest.SchemaVersion != RunManifestSchemaVersion || !safeName.MatchString(manifest.RunID) || !safeName.MatchString(manifest.Attempt) {
		return RunManifest{}, fmt.Errorf("run manifest: unsupported or incomplete manifest")
	}
	if manifest.Suite == "" || len(manifest.Cases) == 0 || manifest.SourceManifestPath == "" || manifest.SourceManifestSHA256 == "" || manifest.EnvironmentManifestPath == "" || manifest.EnvironmentManifestSHA256 == "" {
		return RunManifest{}, fmt.Errorf("run manifest: missing environment identity")
	}
	hash, err := FileSHA256(manifest.SourceManifestPath)
	if err != nil {
		return RunManifest{}, err
	}
	if hash != manifest.SourceManifestSHA256 {
		return RunManifest{}, fmt.Errorf("run manifest: source manifest hash mismatch")
	}
	environmentHash, err := FileSHA256(manifest.EnvironmentManifestPath)
	if err != nil {
		return RunManifest{}, err
	}
	if environmentHash != manifest.EnvironmentManifestSHA256 {
		return RunManifest{}, fmt.Errorf("run manifest: environment manifest hash mismatch")
	}
	return manifest, nil
}
