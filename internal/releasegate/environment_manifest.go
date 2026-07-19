package releasegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"time"
)

const EnvironmentManifestSchemaVersion = 1

type GateIdentity struct {
	Suite        string   `json:"suite,omitempty"`
	Cases        []string `json:"cases"`
	Definition   string   `json:"definition_sha256"`
	OvtestBinary string   `json:"ovtest_binary_sha256"`
}

// ValidateEnvironmentReplay rejects any substitution in the source, harness
// closure, toolchain, gate, plugins, or non-secret configuration. Run IDs,
// timestamps, and the replay link are intentionally not compared.
func ValidateEnvironmentReplay(current, recorded EnvironmentManifest) error {
	if !sameSources(current.Source.Sources, recorded.Source.Sources) {
		return fmt.Errorf("environment replay: OpenViking source differs from manifest")
	}
	if !sameBaseline(current.Baseline, recorded.Baseline) {
		return fmt.Errorf("environment replay: OpenViking baseline differs from manifest")
	}
	if !reflect.DeepEqual(current.Harnesses.Artifacts, recorded.Harnesses.Artifacts) {
		return fmt.Errorf("environment replay: harness closure differs from manifest")
	}
	if !reflect.DeepEqual(current.Platform, recorded.Platform) {
		return fmt.Errorf("environment replay: platform or toolchain differs from manifest")
	}
	if !reflect.DeepEqual(current.Gate, recorded.Gate) {
		return fmt.Errorf("environment replay: gate definition differs from manifest")
	}
	if !reflect.DeepEqual(current.Plugins, recorded.Plugins) {
		return fmt.Errorf("environment replay: plugin identity differs from manifest")
	}
	if !reflect.DeepEqual(current.Configuration, recorded.Configuration) {
		return fmt.Errorf("environment replay: configuration differs from manifest")
	}
	return nil
}

func sameBaseline(a, b *Manifest) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameSources(a.Sources, b.Sources)
}

func sameSources(a, b []Source) bool {
	if len(a) != len(b) {
		return false
	}
	byName := map[string]Source{}
	for _, source := range a {
		byName[source.Name] = source
	}
	for _, source := range b {
		other, ok := byName[source.Name]
		if !ok || other.Repository != source.Repository || other.Commit != source.Commit {
			return false
		}
	}
	return true
}

type PlatformIdentity struct {
	OS        string            `json:"os"`
	Arch      string            `json:"arch"`
	Toolchain map[string]string `json:"toolchain"`
}

type TrustDecision struct {
	CredentialedExecution bool   `json:"credentialed_execution"`
	Authority             string `json:"authority"`
}

type EnvironmentManifest struct {
	SchemaVersion int                         `json:"schema_version"`
	EnvironmentID string                      `json:"environment_id"`
	CreatedAt     time.Time                   `json:"created_at"`
	Mode          SourceMode                  `json:"mode"`
	ReplayOf      string                      `json:"replay_of,omitempty"`
	ReplaySHA256  string                      `json:"replay_sha256,omitempty"`
	Source        Manifest                    `json:"openviking_source"`
	Baseline      *Manifest                   `json:"openviking_baseline,omitempty"`
	Harnesses     HarnessManifest             `json:"harnesses"`
	Platform      PlatformIdentity            `json:"platform"`
	Gate          GateIdentity                `json:"gate"`
	Trust         TrustDecision               `json:"trust"`
	Plugins       map[string]PluginProvenance `json:"plugins,omitempty"`
	Configuration map[string]string           `json:"configuration"`
}

func NewPlatformIdentity(toolchain map[string]string) PlatformIdentity {
	copyOfTools := make(map[string]string, len(toolchain))
	for name, version := range toolchain {
		copyOfTools[name] = version
	}
	return PlatformIdentity{OS: runtime.GOOS, Arch: runtime.GOARCH, Toolchain: copyOfTools}
}

func CaptureToolchain(ctx context.Context) (map[string]string, error) {
	specs := map[string][]string{
		"git": {"--version"}, "go": {"version"}, "node": {"--version"}, "npm": {"--version"}, "uv": {"--version"},
	}
	out := make(map[string]string, len(specs))
	for name, args := range specs {
		item, err := InspectExecutable(ctx, name, args...)
		if err != nil {
			return nil, fmt.Errorf("inspect %s toolchain: %w", name, err)
		}
		out[name] = item.Version
	}
	return out, nil
}

func WriteEnvironmentManifest(path string, manifest EnvironmentManifest) error {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = EnvironmentManifestSchemaVersion
	}
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}
	if manifest.ReplayOf != "" {
		hash, err := FileSHA256(manifest.ReplayOf)
		if err != nil {
			return err
		}
		manifest.ReplaySHA256 = hash
	}
	if err := validateEnvironmentManifest(manifest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".environment-manifest-*")
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
			return fmt.Errorf("environment manifest already exists: %s", path)
		}
		return err
	}
	return nil
}

func ReadEnvironmentManifest(path string) (EnvironmentManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return EnvironmentManifest{}, err
	}
	var manifest EnvironmentManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return EnvironmentManifest{}, err
	}
	if err := validateEnvironmentManifest(manifest); err != nil {
		return EnvironmentManifest{}, err
	}
	if manifest.ReplayOf != "" {
		hash, err := FileSHA256(manifest.ReplayOf)
		if err != nil {
			return EnvironmentManifest{}, err
		}
		if hash != manifest.ReplaySHA256 {
			return EnvironmentManifest{}, fmt.Errorf("environment manifest: replay identity changed")
		}
	}
	return manifest, nil
}

func validateEnvironmentManifest(manifest EnvironmentManifest) error {
	if manifest.SchemaVersion != EnvironmentManifestSchemaVersion || !safeName.MatchString(manifest.EnvironmentID) || manifest.CreatedAt.IsZero() {
		return fmt.Errorf("environment manifest: unsupported or incomplete")
	}
	if manifest.Mode != SourceLatest && manifest.Mode != SourceManifest {
		return fmt.Errorf("environment manifest: invalid mode")
	}
	if err := validateManifest(manifest.Source); err != nil {
		return fmt.Errorf("environment manifest: %w", err)
	}
	if manifest.Baseline != nil {
		if err := validateManifest(*manifest.Baseline); err != nil {
			return fmt.Errorf("environment manifest: invalid baseline: %w", err)
		}
	}
	if err := validateHarnessManifest(manifest.Harnesses); err != nil {
		return fmt.Errorf("environment manifest: %w", err)
	}
	if manifest.Platform.OS == "" || manifest.Platform.Arch == "" || len(manifest.Platform.Toolchain) == 0 {
		return fmt.Errorf("environment manifest: platform identity is incomplete")
	}
	if manifest.Gate.Definition == "" || manifest.Gate.OvtestBinary == "" || len(manifest.Gate.Cases) == 0 {
		return fmt.Errorf("environment manifest: gate identity is incomplete")
	}
	cases := append([]string(nil), manifest.Gate.Cases...)
	sort.Strings(cases)
	for i := range cases {
		if cases[i] == "" || (i > 0 && cases[i] == cases[i-1]) {
			return fmt.Errorf("environment manifest: invalid case set")
		}
	}
	if !manifest.Trust.CredentialedExecution || manifest.Trust.Authority == "" {
		return fmt.Errorf("environment manifest: credentialed trust decision is missing")
	}
	if len(manifest.Configuration) == 0 {
		return fmt.Errorf("environment manifest: configuration identity is missing")
	}
	return nil
}
