package releasegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"code.byted.org/data-arch/ovtest/internal/cli"
)

type SourceMode string

const (
	SourceLatest   SourceMode = "latest"
	SourceManifest SourceMode = "manifest"

	ManifestSchemaVersion = 1
)

type Repository struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Ref  string `json:"ref"`
}

type Source struct {
	Repository
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetched_at"`
}

type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	RunID         string     `json:"run_id"`
	Mode          SourceMode `json:"source_mode"`
	CreatedAt     time.Time  `json:"created_at"`
	ReplayOf      string     `json:"replay_of,omitempty"`
	Sources       []Source   `json:"sources"`
}

type SourceConfig struct {
	Root         string
	RunID        string
	Mode         SourceMode
	ManifestPath string
	Replay       *Manifest
	Repositories []Repository
}

func DefaultRepositories() []Repository {
	return []Repository{
		{Name: "OpenViking", URL: "https://github.com/volcengine/OpenViking.git", Ref: "refs/heads/main"},
	}
}

// OpenVikingRepository returns the sole source repository owned by this gate.
// Harnesses are acquired through their official stable distribution channels,
// rather than from development branches that public users do not run.
func OpenVikingRepository(ref string) (Repository, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "refs/heads/main"
	}
	repo := Repository{Name: "OpenViking", URL: "https://github.com/volcengine/OpenViking.git", Ref: ref}
	if !validRepository(repo) {
		return Repository{}, fmt.Errorf("release source: unsafe OpenViking ref %q", ref)
	}
	return repo, nil
}

type PreparedSources struct {
	Manifest     Manifest
	ManifestPath string
	Worktrees    map[string]string
	root         string
	mirrors      map[string]string
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var fullCommit = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func PrepareSources(ctx context.Context, cfg SourceConfig) (*PreparedSources, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Root == "" || !safeName.MatchString(cfg.RunID) {
		return nil, fmt.Errorf("release source: root and safe run ID are required")
	}
	if cfg.Mode != SourceLatest && cfg.Mode != SourceManifest {
		return nil, fmt.Errorf("release source: unsupported mode %q", cfg.Mode)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock, err := lockSourceCache(ctx, cfg.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, RunID: cfg.RunID, Mode: cfg.Mode, CreatedAt: time.Now().UTC()}
	if cfg.Mode == SourceManifest {
		if cfg.Replay == nil && cfg.ManifestPath == "" {
			return nil, fmt.Errorf("release source: manifest mode requires a manifest path")
		}
		var input Manifest
		if cfg.Replay != nil {
			input = *cfg.Replay
			if err := validateManifest(input); err != nil {
				return nil, err
			}
		} else {
			input, err = ReadManifest(cfg.ManifestPath)
			if err != nil {
				return nil, err
			}
		}
		manifest.ReplayOf = cfg.ManifestPath
		manifest.Sources = append([]Source(nil), input.Sources...)
	} else {
		seen := map[string]bool{}
		for _, repo := range cfg.Repositories {
			if !validRepository(repo) || seen[repo.Name] {
				return nil, fmt.Errorf("release source: invalid or duplicate repository %q", repo.Name)
			}
			seen[repo.Name] = true
			manifest.Sources = append(manifest.Sources, Source{Repository: repo})
		}
	}
	if len(manifest.Sources) == 0 {
		return nil, fmt.Errorf("release source: no repositories configured")
	}
	sort.Slice(manifest.Sources, func(i, j int) bool { return manifest.Sources[i].Name < manifest.Sources[j].Name })

	runRoot := filepath.Join(cfg.Root, "runs", cfg.RunID)
	p := &PreparedSources{
		Manifest: manifest, ManifestPath: filepath.Join(runRoot, "source-manifest.json"),
		Worktrees: map[string]string{}, root: runRoot, mirrors: map[string]string{},
	}
	if err := os.MkdirAll(filepath.Join(runRoot, "worktrees"), 0o700); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = p.closeUnlocked(context.Background())
		}
	}()

	for i := range p.Manifest.Sources {
		source := &p.Manifest.Sources[i]
		if !validRepository(source.Repository) || (source.Commit != "" && !fullCommit.MatchString(source.Commit)) {
			return nil, fmt.Errorf("release source: invalid manifest repository %q", source.Name)
		}
		mirror := filepath.Join(cfg.Root, "mirrors", source.Name+".git")
		p.mirrors[source.Name] = mirror
		if err := ensureMirror(ctx, mirror, source.URL, cfg.Mode == SourceLatest); err != nil {
			return nil, fmt.Errorf("prepare %s: %w", source.Name, err)
		}
		if cfg.Mode == SourceLatest && !strings.HasPrefix(source.Ref, "refs/heads/") {
			fetchSpec := source.Ref
			if !fullCommit.MatchString(source.Ref) {
				fetchSpec = "+" + source.Ref + ":" + source.Ref
			}
			if _, err := gitOutput(ctx, mirror, "fetch", "--no-tags", "origin", fetchSpec); err != nil {
				return nil, fmt.Errorf("fetch %s %s: %w", source.Name, source.Ref, err)
			}
		}
		if source.Commit == "" {
			sha, err := gitOutput(ctx, mirror, "rev-parse", "--verify", source.Ref+"^{commit}")
			if err != nil {
				return nil, fmt.Errorf("resolve %s %s: %w", source.Name, source.Ref, err)
			}
			source.Commit = sha
		} else if _, err := gitOutput(ctx, mirror, "cat-file", "-e", source.Commit+"^{commit}"); err != nil {
			return nil, fmt.Errorf("manifest commit unavailable for %s: %w", source.Name, err)
		}
		if !fullCommit.MatchString(source.Commit) {
			return nil, fmt.Errorf("resolve %s: Git returned a non-full commit ID", source.Name)
		}
		if source.FetchedAt.IsZero() {
			source.FetchedAt = time.Now().UTC()
		}
		protected := "refs/ovtest/runs/" + cfg.RunID + "/" + source.Name
		if _, err := gitOutput(ctx, mirror, "update-ref", protected, source.Commit); err != nil {
			return nil, fmt.Errorf("protect %s: %w", source.Name, err)
		}
		worktree := filepath.Join(runRoot, "worktrees", source.Name)
		if _, err := gitOutput(ctx, mirror, "worktree", "add", "--detach", worktree, source.Commit); err != nil {
			return nil, fmt.Errorf("worktree %s: %w", source.Name, err)
		}
		p.Worktrees[source.Name] = worktree
	}
	if err := WriteManifest(p.ManifestPath, p.Manifest); err != nil {
		return nil, err
	}
	ok = true
	return p, nil
}

func ensureMirror(ctx context.Context, mirror, remote string, fetch bool) error {
	if _, err := os.Stat(mirror); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
			return err
		}
		// Exact replay needs commits and trees, not every historical blob. A
		// promisor mirror keeps refs fully addressable while fetching only blobs
		// required by the detached worktree/build being exercised.
		if _, err = command(ctx, "git", "clone", "--mirror", "--filter=blob:none", remote, mirror); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := gitOutput(ctx, mirror, "remote", "set-url", "origin", remote); err != nil {
		return err
	}
	// A mirror clone starts with +refs/*:refs/* and remote.origin.mirror=true.
	// Leaving that configuration in place would let `fetch --prune` delete our
	// refs/ovtest/runs/* retention anchors because they do not exist upstream.
	// Release inputs only resolve canonical branches, so prune that namespace
	// while keeping ovtest-owned refs outside the fetch destination.
	if _, err := gitOutput(ctx, mirror, "config", "remote.origin.mirror", "false"); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, mirror, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
		return err
	}
	if fetch {
		_, err := gitOutput(ctx, mirror, "fetch", "--prune", "origin")
		return err
	}
	return nil
}

func (p *PreparedSources) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	root := filepath.Dir(filepath.Dir(p.root))
	unlock, err := lockSourceCache(ctx, root)
	if err != nil {
		return err
	}
	defer unlock()
	return p.closeUnlocked(ctx)
}

func (p *PreparedSources) closeUnlocked(ctx context.Context) error {
	var errs []error
	names := make([]string, 0, len(p.Worktrees))
	for name := range p.Worktrees {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		path := p.Worktrees[name]
		if mirror := p.mirrors[name]; mirror != "" {
			if _, err := gitOutput(ctx, mirror, "worktree", "remove", "--force", path); err != nil && !errors.Is(err, context.Canceled) {
				errs = append(errs, err)
			}
		}
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validRepository(repo Repository) bool {
	if !safeName.MatchString(repo.Name) || repo.URL == "" || !safeRef(repo.Ref) {
		return false
	}
	parsed, err := url.Parse(repo.URL)
	return err == nil && parsed.User == nil
}

func safeRef(ref string) bool {
	if fullCommit.MatchString(ref) {
		return true
	}
	isBranch := strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/")
	isPull := false
	parts := strings.Split(ref, "/")
	if len(parts) == 4 && parts[0] == "refs" && parts[1] == "pull" && (parts[3] == "head" || parts[3] == "merge") && parts[2] != "" {
		isPull = true
		for _, r := range parts[2] {
			if r < '0' || r > '9' {
				isPull = false
				break
			}
		}
	}
	if !isBranch && !isPull {
		return false
	}
	if strings.IndexFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	return !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.Contains(ref, "@{") && !strings.ContainsAny(ref, "\\~^:?*[") &&
		!strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".") && !strings.HasSuffix(ref, ".lock")
}

func ReadManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	if err := validateManifest(m); err != nil {
		return Manifest{}, fmt.Errorf("read manifest: unsupported or incomplete manifest")
	}
	return m, nil
}

func WriteManifest(path string, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".manifest-*")
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
			return fmt.Errorf("manifest already exists: %s", path)
		}
		return err
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || !safeName.MatchString(manifest.RunID) || len(manifest.Sources) == 0 {
		return fmt.Errorf("manifest: unsupported or incomplete")
	}
	seen := map[string]bool{}
	for _, source := range manifest.Sources {
		if !validRepository(source.Repository) || !fullCommit.MatchString(source.Commit) || source.FetchedAt.IsZero() || seen[source.Name] {
			return fmt.Errorf("manifest: invalid source %q", source.Name)
		}
		seen[source.Name] = true
	}
	return nil
}

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func gitOutput(ctx context.Context, gitDir string, args ...string) (string, error) {
	argv := append([]string{"git", "--git-dir", gitDir}, args...)
	return command(ctx, argv...)
}

func command(ctx context.Context, argv ...string) (string, error) {
	res := cli.RunContext(ctx, argv, nil, 0, 0)
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s: exit %d: %s", strings.Join(argv, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}
