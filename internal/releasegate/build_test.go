package releasegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPreparedSourcesRecordsArtifactProvenance(t *testing.T) {
	t.Setenv("OPENVIKING_API_KEY", "must-not-leak")
	t.Setenv("OPENVIKING_LLM_BASE_URL", "must-not-influence-build")
	t.Setenv("OPENVIKING_LLM_MODEL", "must-not-influence-build")
	t.Setenv("SOME_TOKEN", "must-not-leak")
	t.Setenv("SSH_AUTH_SOCK", "/must/not/leak")
	t.Setenv("GIT_ASKPASS", "/must/not/leak")
	t.Setenv("NPM_CONFIG_USERCONFIG", "/must/not/leak")
	t.Setenv("PIP_CONFIG_FILE", "/must/not/leak")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/must/not/leak")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/must/not/leak")
	t.Setenv("UNRELATED_SESSION_CREDENTIAL", "must-not-leak")
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared := &PreparedSources{Worktrees: map[string]string{"demo": worktree}}
	buildHomeRoot := filepath.Join(root, "runtime", "candidate-build-homes")
	got, err := BuildPreparedSources(context.Background(), prepared, filepath.Join(root, "evidence"), buildHomeRoot, []BuildSpec{{
		Name: "demo", Commands: [][]string{{"sh", "-c", `
			test -n "$PATH" &&
			test -z "$OPENVIKING_API_KEY" &&
			test -z "$OPENVIKING_LLM_BASE_URL" &&
			test -z "$OPENVIKING_LLM_MODEL" &&
			test -z "$SOME_TOKEN" &&
			test -z "$SSH_AUTH_SOCK" &&
			test -z "$GIT_ASKPASS" &&
			test -z "$NPM_CONFIG_USERCONFIG" &&
			test -z "$PIP_CONFIG_FILE" &&
			test -z "$AWS_SHARED_CREDENTIALS_FILE" &&
			test -z "$GOOGLE_APPLICATION_CREDENTIALS" &&
			test -z "$UNRELATED_SESSION_CREDENTIAL" &&
			printf built > artifact.bin && printf %s "$HOME" > home.txt
		`}},
		Artifacts: map[string]string{"binary": "artifact.bin", "home": "home.txt"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got["demo"].Artifacts["binary"].SHA256 == "" || !filepath.IsAbs(got["demo"].Artifacts["binary"].Path) {
		t.Fatalf("build provenance = %+v", got)
	}
	home := mustRead(t, filepath.Join(worktree, "home.txt"))
	if home == os.Getenv("HOME") || home != filepath.Join(buildHomeRoot, "demo") {
		t.Fatalf("build HOME was not isolated: %q", home)
	}
}

func TestBuildBaseEnvIsAnAllowlist(t *testing.T) {
	got := buildBaseEnv([]string{
		"PATH=/bin", "LANG=en_US.UTF-8", "SSL_CERT_FILE=/certs.pem",
		"HOME=/home/operator", "XDG_CONFIG_HOME=/config", "SSH_AUTH_SOCK=/agent",
		"GIT_ASKPASS=/askpass", "NPM_CONFIG_USERCONFIG=/npmrc", "PIP_CONFIG_FILE=/pip.conf",
		"AWS_SHARED_CREDENTIALS_FILE=/aws", "GOOGLE_APPLICATION_CREDENTIALS=/gcp",
		"SOME_UNRECOGNIZED_SECRET=value",
	})
	want := []string{"PATH=/bin", "LANG=en_US.UTF-8", "SSL_CERT_FILE=/certs.pem"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("buildBaseEnv() = %q, want %q", got, want)
	}
}

func TestBuildRejectsArtifactOutsideWorktree(t *testing.T) {
	root := t.TempDir()
	prepared := &PreparedSources{Worktrees: map[string]string{"demo": root}}
	_, err := BuildPreparedSources(context.Background(), prepared, t.TempDir(), t.TempDir(), []BuildSpec{{
		Name: "demo", Artifacts: map[string]string{"bad": "../outside"},
	}})
	if err == nil {
		t.Fatal("outside artifact accepted")
	}
}
