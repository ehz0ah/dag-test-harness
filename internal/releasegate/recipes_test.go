package releasegate

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBuildSpecsAreCompleteAndCredentialFree(t *testing.T) {
	root := t.TempDir()
	specs, err := DefaultBuildSpecs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d build specs, want 1", len(specs))
	}
	want := map[string]bool{"OpenViking": true}
	for _, spec := range specs {
		if !want[spec.Name] {
			t.Fatalf("unexpected source recipe %q", spec.Name)
		}
		delete(want, spec.Name)
		if len(spec.Commands) == 0 || len(spec.Artifacts) == 0 {
			t.Fatalf("recipe %q is incomplete", spec.Name)
		}
		for key := range spec.Env {
			if key == "OPENVIKING_API_KEY" || key == "OPENAI_API_KEY" {
				t.Fatalf("recipe %q contains runtime credential %q", spec.Name, key)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing recipes: %v", want)
	}
	if got := specs[0].Env["UV_CACHE_DIR"]; got != filepath.Join(root, "uv") {
		t.Fatalf("UV cache = %q", got)
	}
	commands := joinedCommands(specs[0].Commands)
	for _, want := range []string{
		"npm --prefix examples/openclaw-plugin ci --ignore-scripts --no-audit --no-fund",
		"npm --prefix examples/openclaw-plugin run build",
		"npm --prefix examples/openclaw-plugin prune --omit=dev --ignore-scripts --no-audit --no-fund",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("OpenViking build does not prepare OpenClaw plugin %q: %s", want, commands)
		}
	}
	for _, artifact := range []string{"openclaw-plugin", "openclaw-plugin-setup"} {
		if specs[0].Artifacts[artifact] == "" {
			t.Fatalf("OpenViking build does not verify artifact %q", artifact)
		}
	}
}

func TestBuiltArtifact(t *testing.T) {
	builds := map[string]BuildProvenance{"repo": {Artifacts: map[string]ArtifactProvenance{
		"bin": {Path: "/tmp/bin", SHA256: "abc"},
	}}}
	if got, err := BuiltArtifact(builds, "repo", "bin"); err != nil || got != "/tmp/bin" {
		t.Fatalf("BuiltArtifact = %q, %v", got, err)
	}
	if _, err := BuiltArtifact(builds, "repo", "missing"); err == nil {
		t.Fatal("missing artifact should fail")
	}
}

func joinedCommands(commands [][]string) string {
	parts := []string{}
	for _, command := range commands {
		parts = append(parts, strings.Join(command, " "))
	}
	return strings.Join(parts, "\n")
}
