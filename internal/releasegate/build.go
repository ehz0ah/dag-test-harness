package releasegate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type BuildSpec struct {
	Name      string
	Commands  [][]string
	Env       map[string]string
	Artifacts map[string]string
}

type ArtifactProvenance struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type BuildProvenance struct {
	LogPath   string                        `json:"log_path"`
	Artifacts map[string]ArtifactProvenance `json:"artifacts"`
}

func BuildPreparedSources(ctx context.Context, prepared *PreparedSources, evidenceDir, buildHomeRoot string, specs []BuildSpec) (map[string]BuildProvenance, error) {
	if prepared == nil || evidenceDir == "" || buildHomeRoot == "" {
		return nil, fmt.Errorf("build sources: prepared sources, evidence directory, and build home root are required")
	}
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(buildHomeRoot, 0o700); err != nil {
		return nil, err
	}
	result := map[string]BuildProvenance{}
	seen := map[string]bool{}
	for _, spec := range specs {
		worktree, ok := prepared.Worktrees[spec.Name]
		if !ok || seen[spec.Name] {
			return nil, fmt.Errorf("build sources: unknown or duplicate source %q", spec.Name)
		}
		seen[spec.Name] = true
		for key := range spec.Env {
			upper := strings.ToUpper(key)
			if strings.Contains(upper, "API_KEY") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") {
				return nil, fmt.Errorf("build %s: runtime credential %q is not allowed", spec.Name, key)
			}
		}
		buildHome := filepath.Join(buildHomeRoot, spec.Name)
		if err := os.MkdirAll(buildHome, 0o700); err != nil {
			return nil, err
		}
		isolatedHome := map[string]string{
			"HOME": buildHome, "XDG_CONFIG_HOME": filepath.Join(buildHome, "config"),
			"XDG_DATA_HOME": filepath.Join(buildHome, "data"), "XDG_STATE_HOME": filepath.Join(buildHome, "state"),
			"XDG_CACHE_HOME": filepath.Join(buildHome, "cache"),
		}
		logPath := filepath.Join(evidenceDir, "build-"+spec.Name+".log")
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		for _, argv := range spec.Commands {
			if len(argv) == 0 {
				log.Close()
				return nil, fmt.Errorf("build %s: empty command", spec.Name)
			}
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = worktree
			prepareManagedCommand(cmd)
			cmd.Cancel = func() error { return killManagedCommand(cmd) }
			cmd.Env = mergeProcessEnv(mergeProcessEnv(buildBaseEnv(os.Environ()), spec.Env), isolatedHome)
			var stderr bytes.Buffer
			cmd.Stdout = log
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				_, _ = log.Write(stderr.Bytes())
				log.Close()
				return nil, fmt.Errorf("build %s (%s): %w: %s", spec.Name, strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
			}
			_, _ = log.Write(stderr.Bytes())
		}
		if err := log.Close(); err != nil {
			return nil, err
		}
		provenance := BuildProvenance{LogPath: logPath, Artifacts: map[string]ArtifactProvenance{}}
		for name, relative := range spec.Artifacts {
			if filepath.IsAbs(relative) {
				return nil, fmt.Errorf("build %s: artifact %q must be relative", spec.Name, name)
			}
			path := filepath.Clean(filepath.Join(worktree, relative))
			rel, err := filepath.Rel(worktree, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("build %s: artifact %q escapes worktree", spec.Name, name)
			}
			hash, err := FileSHA256(path)
			if err != nil {
				return nil, fmt.Errorf("build %s artifact %q: %w", spec.Name, name, err)
			}
			provenance.Artifacts[name] = ArtifactProvenance{Path: path, SHA256: hash}
		}
		result[spec.Name] = provenance
	}
	return result, nil
}

func buildBaseEnv(environ []string) []string {
	// Credentialed release-gate runs often start from an operator's daily shell.
	// Pass only process-launch and TLS settings that build tools and managed
	// services may legitimately need. Everything else must be supplied by the
	// stage that owns it; a denylist cannot anticipate every credential name or
	// configuration-file pointer.
	allowed := map[string]bool{
		"PATH": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"SYSTEMROOT": true, "COMSPEC": true, "PATHEXT": true,
		"DEVELOPER_DIR": true, "SDKROOT": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	}
	out := make([]string, 0, len(allowed))
	for _, item := range environ {
		key, _, _ := strings.Cut(item, "=")
		if allowed[strings.ToUpper(key)] {
			out = append(out, item)
		}
	}
	return out
}
