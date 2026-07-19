package releasegate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type TargetMode string

const (
	TargetLocal    TargetMode = "local"
	TargetExternal TargetMode = "external"
)

type ManagedServiceConfig struct {
	Command        []string
	Env            map[string]string
	URL            string
	ReadyPath      string
	HealthPath     string
	APIKey         string
	StartupTimeout time.Duration
	LogPath        string
}

type ManagedService struct {
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	mu       sync.Mutex
	log      *os.File
	stopping bool
}

func StartManagedService(ctx context.Context, cfg ManagedServiceConfig) (*ManagedService, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(cfg.Command) == 0 || cfg.URL == "" || cfg.ReadyPath == "" || cfg.HealthPath == "" || cfg.LogPath == "" {
		return nil, fmt.Errorf("managed service: command, URL, probes, and log path are required")
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 60 * time.Second
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	prepareManagedCommand(cmd)
	cmd.Cancel = func() error { return killManagedCommand(cmd) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = mergeProcessEnv(buildBaseEnv(os.Environ()), cfg.Env)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	s := &ManagedService{cmd: cmd, done: make(chan struct{}), log: log}
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.waitErr = err
		if ctx.Err() != nil {
			s.stopping = true
		}
		s.mu.Unlock()
		_ = log.Close()
		close(s.done)
	}()

	startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	if err := waitForTarget(startupCtx, cfg.URL, cfg.ReadyPath, cfg.HealthPath, cfg.APIKey, s.done); err != nil {
		_ = s.Stop(context.Background(), 5*time.Second)
		return nil, fmt.Errorf("managed service startup: %w (log: %s)", err, cfg.LogPath)
	}
	return s, nil
}

func (s *ManagedService) Done() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

func (s *ManagedService) Stop(ctx context.Context, timeout time.Duration) error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	_ = terminateManagedCommand(s.cmd)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		_ = killManagedCommand(s.cmd)
		return s.waitAfterKill(ctx.Err())
	case <-timer.C:
		_ = killManagedCommand(s.cmd)
		return s.waitAfterKill(fmt.Errorf("managed service did not stop within %s", timeout))
	}
}

func (s *ManagedService) waitAfterKill(cause error) error {
	const postKillTimeout = 5 * time.Second
	timer := time.NewTimer(postKillTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return cause
	case <-timer.C:
		return errors.Join(cause, fmt.Errorf("managed service did not exit within %s after kill", postKillTimeout))
	}
}

func (s *ManagedService) UnexpectedExit() error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.stopping {
			if s.waitErr != nil {
				return fmt.Errorf("managed service exited unexpectedly: %w", s.waitErr)
			}
			return fmt.Errorf("managed service exited unexpectedly")
		}
	default:
	}
	return nil
}

type LocalOpenVikingConfig struct {
	ServerBin      string
	TemplateConfig string
	RuntimeDir     string
	SecretsDir     string
	EvidenceDir    string
	Env            map[string]string
	StartupTimeout time.Duration
	AccountID      string
	UserID         string
}

type LocalOpenViking struct {
	Service        *ManagedService
	URL            string
	RootAPIKey     string
	UserAPIKey     string
	ConfigPath     string
	UserConfigPath string
}

func StartLocalOpenViking(ctx context.Context, cfg LocalOpenVikingConfig) (*LocalOpenViking, error) {
	if cfg.ServerBin == "" || cfg.TemplateConfig == "" || cfg.RuntimeDir == "" || cfg.SecretsDir == "" || cfg.EvidenceDir == "" {
		return nil, fmt.Errorf("local OpenViking: server, template, runtime, secrets, and evidence paths are required")
	}
	rootKey, err := randomSecret(32)
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(cfg.SecretsDir, "openviking", "ov.conf")
	workspace := filepath.Join(cfg.RuntimeDir, "openviking", "storage")
	if err := WriteLocalOpenVikingConfig(cfg.TemplateConfig, configPath, workspace, rootKey); err != nil {
		return nil, err
	}
	var baseURL string
	var service *ManagedService
	for attempt := 0; attempt < 3; attempt++ {
		port, portErr := freeLoopbackPort()
		if portErr != nil {
			return nil, portErr
		}
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		logName := "openviking-server.log"
		if attempt > 0 {
			logName = fmt.Sprintf("openviking-server-%d.log", attempt+1)
		}
		logPath := filepath.Join(cfg.EvidenceDir, logName)
		service, err = StartManagedService(ctx, ManagedServiceConfig{
			Command: []string{cfg.ServerBin, "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--config", configPath},
			Env:     cfg.Env, URL: baseURL, ReadyPath: "/ready", HealthPath: "/health", APIKey: rootKey,
			StartupTimeout: cfg.StartupTimeout, LogPath: logPath,
		})
		if err == nil {
			break
		}
		if !isBindCollision(logPath) || attempt == 2 {
			return nil, err
		}
	}
	accountID := cfg.AccountID
	if accountID == "" {
		accountID = "ovtest"
	}
	userID := cfg.UserID
	if userID == "" {
		userID = "runner"
	}
	userKey, userConfig, err := bootstrapLocalUser(ctx, cfg.SecretsDir, baseURL, rootKey, accountID, userID)
	if err != nil {
		_ = service.Stop(context.Background(), 5*time.Second)
		return nil, err
	}
	if err := validateUserIdentity(ctx, baseURL, userKey, userID); err != nil {
		_ = service.Stop(context.Background(), 5*time.Second)
		return nil, err
	}
	if err := validateWrongKeyRejected(ctx, baseURL, "ovtest-deliberately-wrong-key"); err != nil {
		_ = service.Stop(context.Background(), 5*time.Second)
		return nil, err
	}
	return &LocalOpenViking{Service: service, URL: baseURL, RootAPIKey: rootKey, UserAPIKey: userKey, ConfigPath: configPath, UserConfigPath: userConfig}, nil
}

func isBindCollision(logPath string) bool {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}
	text := strings.ToLower(string(raw))
	return strings.Contains(text, "address already in use") || strings.Contains(text, "port is already in use")
}

func WriteLocalOpenVikingConfig(templatePath, destination, workspace, rootAPIKey string) error {
	if templatePath == "" || destination == "" || workspace == "" || rootAPIKey == "" {
		return fmt.Errorf("local OpenViking config: all arguments are required")
	}
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("local OpenViking config: %w", err)
	}
	storage, _ := config["storage"].(map[string]any)
	if storage == nil {
		storage = map[string]any{}
		config["storage"] = storage
	}
	storage["workspace"] = workspace
	server, _ := config["server"].(map[string]any)
	if server == nil {
		server = map[string]any{}
		config["server"] = server
	}
	server["host"] = "127.0.0.1"
	server["workers"] = 1
	server["auth_mode"] = "api_key"
	server["root_api_key"] = rootAPIKey
	if vlm, ok := config["vlm"].(map[string]any); ok && vlm["api_key"] != nil {
		vlm["api_key"] = "$OPENVIKING_LLM_API_KEY"
	}
	if embedding, ok := config["embedding"].(map[string]any); ok {
		if dense, ok := embedding["dense"].(map[string]any); ok && dense["api_key"] != nil {
			dense["api_key"] = "$OPENVIKING_EMBEDDING_API_KEY"
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(destination, out, 0o600)
}

func ValidateExternalTarget(ctx context.Context, baseURL, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("external OpenViking requires OV_TEST_OPENVIKING_API_KEY")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("external OpenViking URL is invalid")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	baseURL = strings.TrimRight(baseURL, "/")
	if err := waitForReady(probeCtx, baseURL+"/ready", nil); err != nil {
		return err
	}
	if err := validateUserIdentity(probeCtx, baseURL, apiKey, ""); err != nil {
		return err
	}
	return validateWrongKeyRejected(probeCtx, baseURL, "ovtest-deliberately-wrong-key")
}

func waitForReady(ctx context.Context, endpoint string, processDone <-chan struct{}) error {
	// /ready performs a real embedding-provider probe. Keep retries
	// conservative so startup validation does not create provider load itself.
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var last error
	for {
		if err := probeJSON(ctx, endpoint, "", "ready", 15*time.Second); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-processDone:
			return fmt.Errorf("process exited before readiness: %w", last)
		case <-ticker.C:
		}
	}
}

func bootstrapLocalUser(ctx context.Context, runtimeDir, baseURL, rootKey, accountID, userID string) (string, string, error) {
	body, err := json.Marshal(map[string]any{"account_id": accountID, "admin_user_id": userID})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/admin/accounts", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", rootKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("bootstrap local OpenViking user returned HTTP %d", resp.StatusCode)
	}
	var envelope map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return "", "", err
	}
	result, _ := envelope["result"].(map[string]any)
	userKey := fmt.Sprint(result["user_key"])
	if userKey == "" || userKey == "<nil>" {
		return "", "", fmt.Errorf("bootstrap local OpenViking user response omitted user_key")
	}
	confPath := filepath.Join(runtimeDir, "openviking", "ovcli.conf")
	conf, err := json.MarshalIndent(map[string]any{"url": baseURL, "api_key": userKey, "output": "json", "echo_command": false}, "", "  ")
	if err != nil {
		return "", "", err
	}
	conf = append(conf, '\n')
	if err := os.MkdirAll(filepath.Dir(confPath), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		return "", "", err
	}
	return userKey, confPath, nil
}

func validateUserIdentity(ctx context.Context, baseURL, apiKey, expectedUser string) error {
	body, status, err := healthIdentity(ctx, baseURL, apiKey)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("authenticated OpenViking health returned HTTP %d", status)
	}
	userID := fmt.Sprint(body["user_id"])
	role := fmt.Sprint(body["role"])
	if userID == "" || userID == "<nil>" || role == "" || role == "<nil>" {
		return fmt.Errorf("OpenViking API key was not resolved to an identity")
	}
	if expectedUser != "" && userID != expectedUser {
		return fmt.Errorf("OpenViking API key resolved user %q, want %q", userID, expectedUser)
	}
	return nil
}

func validateWrongKeyRejected(ctx context.Context, baseURL, wrongKey string) error {
	body, status, err := healthIdentity(ctx, baseURL, wrongKey)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil
	}
	if status != http.StatusOK {
		return fmt.Errorf("wrong-key probe returned unexpected HTTP %d", status)
	}
	if body["user_id"] != nil || body["role"] != nil || body["account_id"] != nil {
		return fmt.Errorf("OpenViking accepted a deliberately wrong API key")
	}
	return nil
}

func healthIdentity(ctx context.Context, baseURL, apiKey string) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var body map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, resp.StatusCode, err
		}
	}
	return body, resp.StatusCode, nil
}

func waitForTarget(ctx context.Context, baseURL, readyPath, healthPath, apiKey string, processDone <-chan struct{}) error {
	// Wait for the cheap authenticated health endpoint before invoking the deep
	// readiness endpoint, which currently performs an embedding request.
	healthTicker := time.NewTicker(250 * time.Millisecond)
	defer healthTicker.Stop()
	var healthErr error
	for {
		healthErr = probeJSON(ctx, baseURL+healthPath, apiKey, "ok", 2*time.Second)
		if healthErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), healthErr)
		case <-processDone:
			return fmt.Errorf("process exited before health: %w", healthErr)
		case <-healthTicker.C:
		}
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var last error
	for {
		if err := probeJSON(ctx, baseURL+readyPath, "", "ready", 15*time.Second); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-processDone:
			return fmt.Errorf("process exited before readiness: %w", last)
		case <-ticker.C:
		}
	}
}

func probeJSON(ctx context.Context, endpoint, apiKey, expectedStatus string, timeout time.Duration) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", endpoint, resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if fmt.Sprint(body["status"]) != expectedStatus {
		return fmt.Errorf("%s status is %q", endpoint, body["status"])
	}
	if apiKey != "" && body["auth_mode"] != nil && fmt.Sprint(body["auth_mode"]) != "api_key" {
		return fmt.Errorf("%s auth_mode is %q", endpoint, body["auth_mode"])
	}
	return nil
}

func mergeProcessEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if _, replace := extra[key]; !replace {
			out = append(out, item)
		}
	}
	for key, value := range extra {
		out = append(out, key+"="+value)
	}
	return out
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func randomSecret(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
