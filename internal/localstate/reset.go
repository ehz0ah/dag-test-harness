package localstate

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reset removes ovtest-owned runtime state while preserving reusable config.
// It is intended for isolated local test servers. It refuses to run if the
// configured local OpenViking endpoint is reachable, because deleting an active
// workspace can corrupt the process using it.
func Reset() ([]string, error) {
	stateRoot, err := absPath(stateDir())
	if err != nil {
		return nil, err
	}
	if err := validateStateDir(stateRoot); err != nil {
		return nil, err
	}

	confRoot, err := absPath(confDir())
	if err != nil {
		return nil, err
	}
	if err := refuseReachableOpenViking(openVikingConf(confRoot)); err != nil {
		return nil, err
	}

	targets, err := resetTargets(stateRoot, confRoot)
	if err != nil {
		return nil, err
	}

	removed := []string{}
	for _, target := range targets {
		ok, err := removeExisting(target)
		if err != nil {
			return removed, err
		}
		if ok {
			removed = append(removed, target)
		}
	}
	if err := os.MkdirAll(filepath.Join(stateRoot, "hermes"), 0o755); err != nil {
		return removed, err
	}
	sort.Strings(removed)
	return removed, nil
}

func stateDir() string {
	if v := os.Getenv("OV_TEST_STATE_DIR"); v != "" {
		return v
	}
	return filepath.Clean(filepath.Join("..", ".ovtest"))
}

func confDir() string {
	if v := os.Getenv("OV_TEST_CONF_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openviking")
}

func openVikingConf(confRoot string) string {
	if v := os.Getenv("OV_TEST_OPENVIKING_CONF"); v != "" {
		return v
	}
	return filepath.Join(confRoot, "ov.conf")
}

func resetTargets(stateRoot, confRoot string) ([]string, error) {
	targets := []string{}

	hermesRoot := filepath.Join(stateRoot, "hermes")
	if entries, err := os.ReadDir(hermesRoot); err == nil {
		for _, entry := range entries {
			child := filepath.Join(hermesRoot, entry.Name())
			if entry.Name() == "template" && entry.IsDir() {
				targets = append(targets, hermesTemplateRuntimeTargets(child)...)
				continue
			}
			targets = append(targets, child)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	targets = append(targets, filepath.Join(stateRoot, "fixtures"))
	for _, name := range []string{"cache", "data", "sessions", "state", "storage", "temp", "tmp"} {
		targets = append(targets, filepath.Join(stateRoot, name))
	}

	workspace, err := configuredWorkspace(openVikingConf(confRoot))
	if err != nil {
		return nil, err
	}
	if workspace != "" {
		workspace, err = absPath(workspace)
		if err != nil {
			return nil, err
		}
		inside, err := isStrictChildResolved(stateRoot, workspace)
		if err != nil {
			return nil, err
		}
		if !inside {
			return nil, fmt.Errorf("refusing to reset OpenViking workspace outside state dir: %s", workspace)
		}
		targets = append(targets, workspace)
	}
	return dedupeTargets(targets), nil
}

func hermesTemplateRuntimeTargets(template string) []string {
	names := []string{
		"audio_cache",
		"cache",
		"cron",
		"data",
		"image_cache",
		"logs",
		"memories",
		"pairing",
		"sandboxes",
		"sessions",
		"state",
		"state.db",
		"storage",
		"temp",
		"tmp",
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, filepath.Join(template, name))
	}
	return out
}

func configuredWorkspace(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var conf struct {
		Storage struct {
			Workspace string `json:"workspace"`
		} `json:"storage"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return "", fmt.Errorf("read OpenViking config %s: %w", path, err)
	}
	return conf.Storage.Workspace, nil
}

func refuseReachableOpenViking(path string) error {
	rawURL, err := configuredOpenVikingURL(path)
	if err != nil {
		return err
	}
	reachable, err := probeOpenViking(rawURL)
	if err != nil {
		return err
	}
	if reachable {
		return fmt.Errorf("configured local OpenViking endpoint is reachable at %s; stop it before reset-local-state", rawURL)
	}
	return nil
}

func configuredOpenVikingURL(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var conf struct {
		URL    string `json:"url"`
		Server struct {
			Host string `json:"host"`
			Port any    `json:"port"`
		} `json:"server"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return "", fmt.Errorf("read OpenViking config %s: %w", path, err)
	}
	if strings.TrimSpace(conf.URL) != "" {
		return strings.TrimSpace(conf.URL), nil
	}
	host := strings.TrimSpace(conf.Server.Host)
	port := portString(conf.Server.Port)
	if host == "" || port == "" {
		return "", nil
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func portString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x <= 0 || x != float64(int(x)) {
			return ""
		}
		return strconv.Itoa(int(x))
	case int:
		if x <= 0 {
			return ""
		}
		return strconv.Itoa(x)
	}
	return ""
}

var probeTimeout = 2 * time.Second

var probeOpenViking = func(rawURL string) (bool, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false, fmt.Errorf("parse OpenViking URL %q: %w", rawURL, err)
	}
	host := parsed.Hostname()
	if !isLocalHost(host) {
		return false, nil
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get(strings.TrimRight(rawURL, "/") + "/health")
	if err != nil {
		if isConnectionRefused(err) {
			return false, nil
		}
		return false, fmt.Errorf("probe local OpenViking endpoint %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return true, nil
}

func isConnectionRefused(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "connection refused") ||
		strings.Contains(text, "actively refused")
}

func isLocalHost(host string) bool {
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateStateDir(path string) error {
	if path == "" || path == string(filepath.Separator) {
		return fmt.Errorf("refusing to reset unsafe state dir: %q", path)
	}
	home, _ := os.UserHomeDir()
	if home != "" && path == home {
		return fmt.Errorf("refusing to reset home directory as state dir: %s", path)
	}
	return nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	return filepath.Abs(path)
}

func isStrictChild(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isStrictChildResolved(parent, child string) (bool, error) {
	realParent, err := evalPath(parent)
	if err != nil {
		return false, err
	}
	realChild, err := evalPath(child)
	if err != nil {
		return false, err
	}
	return isStrictChild(realParent, realChild), nil
}

func evalPath(path string) (string, error) {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	missing := []string{}
	for cur := path; ; cur = filepath.Dir(cur) {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{real}, missing...)...), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path, nil
		}
		missing = append([]string{filepath.Base(cur)}, missing...)
	}
}

func dedupeTargets(targets []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		target = filepath.Clean(target)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

func removeExisting(path string) (bool, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	return true, nil
}
