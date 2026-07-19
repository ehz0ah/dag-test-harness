package releasegate

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultBuildSpecs describes the canonical, credential-free builds used by
// the release gate. Dependency caches live outside disposable worktrees; every
// produced artifact remains inside the source worktree pinned by the manifest.
func DefaultBuildSpecs(cacheRoot string, prepared ...*PreparedSources) ([]BuildSpec, error) {
	if cacheRoot == "" {
		return nil, fmt.Errorf("build recipes: cache root is required")
	}
	venvBin := ".venv/bin"
	serverBin := filepath.Join(venvBin, "openviking-server")
	ovBin := filepath.Join(venvBin, "ov")
	if runtime.GOOS == "windows" {
		venvBin = `.venv\Scripts`
		serverBin = filepath.Join(venvBin, "openviking-server.exe")
		ovBin = filepath.Join(venvBin, "ov.exe")
	}

	return []BuildSpec{
		{
			Name: "OpenViking",
			Commands: [][]string{
				{"uv", "sync", "--frozen", "--native-tls"},
				{"npm", "--prefix", "examples/openclaw-plugin", "ci", "--ignore-scripts", "--no-audit", "--no-fund"},
				{"npm", "--prefix", "examples/openclaw-plugin", "run", "build"},
				{"npm", "--prefix", "examples/openclaw-plugin", "prune", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund"},
			},
			Env: map[string]string{
				"UV_CACHE_DIR": filepath.Join(cacheRoot, "uv"), "npm_config_cache": filepath.Join(cacheRoot, "npm"),
			},
			Artifacts: map[string]string{
				"server": serverBin, "ov": ovBin,
				"openclaw-plugin":       filepath.Join("examples", "openclaw-plugin", "dist", "index.js"),
				"openclaw-plugin-setup": filepath.Join("examples", "openclaw-plugin", "dist", "commands", "setup.js"),
			},
		},
	}, nil
}

func ValidateBuildTools() error {
	missing := []string{}
	for _, binary := range []string{"git", "uv", "node", "npm"} {
		if _, err := exec.LookPath(binary); err != nil {
			missing = append(missing, binary)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("release build prerequisites are missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func BuiltArtifact(builds map[string]BuildProvenance, source, artifact string) (string, error) {
	build, ok := builds[source]
	if !ok {
		return "", fmt.Errorf("build artifact: source %q was not built", source)
	}
	item, ok := build.Artifacts[artifact]
	if !ok || item.Path == "" {
		return "", fmt.Errorf("build artifact: %s/%s is unavailable", source, artifact)
	}
	return item.Path, nil
}
