package releasegate

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteLocalOpenVikingConfigIsolatedAPIKeyMode(t *testing.T) {
	template := filepath.Join(t.TempDir(), "template.json")
	if err := os.WriteFile(template, []byte(`{"storage":{"workspace":"old"},"server":{"workers":4},"vlm":{"provider":"x","api_key":"literal-llm-secret"},"embedding":{"dense":{"api_key":"literal-embed-secret"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "ov.conf")
	if err := WriteLocalOpenVikingConfig(template, dest, "/isolated/workspace", "root-secret"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dest)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	storage := got["storage"].(map[string]any)
	server := got["server"].(map[string]any)
	if storage["workspace"] != "/isolated/workspace" || server["auth_mode"] != "api_key" || server["root_api_key"] != "root-secret" || server["workers"] != float64(1) {
		t.Fatalf("generated config = %v", got)
	}
	if got["vlm"].(map[string]any)["api_key"] != "$OPENVIKING_LLM_API_KEY" || got["embedding"].(map[string]any)["dense"].(map[string]any)["api_key"] != "$OPENVIKING_EMBEDDING_API_KEY" || strings.Contains(string(raw), "literal-") {
		t.Fatal("model config was not preserved")
	}
}

func TestManagedServiceReadinessAndCancellation(t *testing.T) {
	if os.Getenv("OVTEST_SERVICE_HELPER") == "1" {
		runServiceHelper()
		return
	}
	port := reservePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	logPath := filepath.Join(t.TempDir(), "service.log")
	service, err := StartManagedService(ctx, ManagedServiceConfig{
		Command: []string{os.Args[0], "-test.run=TestManagedServiceReadinessAndCancellation", "--", fmt.Sprintf("--listen=127.0.0.1:%d", port)},
		Env:     map[string]string{"OVTEST_SERVICE_HELPER": "1"}, URL: fmt.Sprintf("http://127.0.0.1:%d", port),
		ReadyPath: "/ready", HealthPath: "/health", APIKey: "test-user", StartupTimeout: 5 * time.Second, LogPath: logPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-service.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("service process survived parent cancellation")
	}
	if err := service.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForTargetChecksHealthBeforeDeepReadiness(t *testing.T) {
	var healthCalls atomic.Int32
	var readyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			call := healthCalls.Add(1)
			if call < 2 {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","auth_mode":"api_key"}`))
		case "/ready":
			readyCalls.Add(1)
			if healthCalls.Load() < 2 {
				http.Error(w, "health not established", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := waitForTarget(ctx, server.URL, "/ready", "/health", "root-key", nil); err != nil {
		t.Fatal(err)
	}
	if readyCalls.Load() != 1 {
		t.Fatalf("deep readiness calls = %d, want 1", readyCalls.Load())
	}
}

func TestWaitForTargetAllowsSlowDeepReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok","auth_mode":"api_key"}`))
		case "/ready":
			time.Sleep(2200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForTarget(ctx, server.URL, "/ready", "/health", "root-key", nil); err != nil {
		t.Fatalf("slow but healthy readiness failed: %v", err)
	}
}

func TestExternalTargetRequiresExplicitAPIKeyAndValidatesAuth(t *testing.T) {
	server := http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ready"}`)) })
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "user-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","auth_mode":"api_key","account_id":"a","user_id":"runner","role":"USER"}`))
	})
	server.Handler = mux
	go server.Serve(ln)
	t.Cleanup(func() { _ = server.Close() })
	url := "http://" + ln.Addr().String()
	if err := ValidateExternalTarget(context.Background(), url, ""); err == nil {
		t.Fatal("external target without key accepted")
	}
	if err := ValidateExternalTarget(context.Background(), url, "wrong"); err == nil {
		t.Fatal("external target with wrong key accepted")
	}
	if err := ValidateExternalTarget(context.Background(), url, "user-key"); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapLocalUserUsesRootOnlyForManagement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/accounts" || r.Header.Get("X-API-Key") != "root-key" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"result":{"user_key":"scoped-user-key"}}`))
	}))
	defer server.Close()
	secretDir := t.TempDir()
	key, conf, err := bootstrapLocalUser(context.Background(), secretDir, server.URL, "root-key", "acct", "runner")
	if err != nil {
		t.Fatal(err)
	}
	if key != "scoped-user-key" {
		t.Fatalf("key = %q", key)
	}
	raw, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "root-key") || !strings.Contains(string(raw), "scoped-user-key") {
		t.Fatalf("user config contains wrong credential: %s", raw)
	}
	info, _ := os.Stat(conf)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("user config mode = %o", info.Mode().Perm())
	}
}

func TestManagedServiceClassifiesUnexpectedExit(t *testing.T) {
	if os.Getenv("OVTEST_SERVICE_HELPER") == "1" {
		runServiceHelper()
		return
	}
	port := reservePort(t)
	service, err := StartManagedService(context.Background(), ManagedServiceConfig{
		Command: []string{os.Args[0], "-test.run=TestManagedServiceClassifiesUnexpectedExit", "--", fmt.Sprintf("--listen=127.0.0.1:%d", port)},
		Env:     map[string]string{"OVTEST_SERVICE_HELPER": "1", "OVTEST_SERVICE_EXIT": "1"}, URL: fmt.Sprintf("http://127.0.0.1:%d", port),
		ReadyPath: "/ready", HealthPath: "/health", APIKey: "test-user", StartupTimeout: 5 * time.Second,
		LogPath: filepath.Join(t.TempDir(), "service.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not exit")
	}
	if err := service.UnexpectedExit(); err == nil {
		t.Fatal("unexpected exit was not classified")
	}
}

func TestStartLocalOpenVikingBootstrapsScopedUser(t *testing.T) {
	if os.Getenv("OVTEST_SERVICE_HELPER") == "1" {
		runServiceHelper()
		return
	}
	root := t.TempDir()
	template := filepath.Join(root, "template.json")
	if err := os.WriteFile(template, []byte(`{"storage":{},"server":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "openviking-server")
	script := "#!/bin/sh\nexec \"$OVTEST_HELPER_BIN\" -test.run=TestStartLocalOpenVikingBootstrapsScopedUser -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	local, err := StartLocalOpenViking(context.Background(), LocalOpenVikingConfig{
		ServerBin: wrapper, TemplateConfig: template, RuntimeDir: filepath.Join(root, "runtime"),
		SecretsDir: filepath.Join(root, "secrets"), EvidenceDir: filepath.Join(root, "evidence"),
		Env: map[string]string{"OVTEST_SERVICE_HELPER": "1", "OVTEST_HELPER_BIN": os.Args[0]}, StartupTimeout: 5 * time.Second,
		AccountID: "acct", UserID: "runner",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Service.Stop(context.Background(), time.Second)
	if local.UserAPIKey != "scoped-user-key" || local.UserAPIKey == local.RootAPIKey {
		t.Fatalf("local keys root=%q user=%q", local.RootAPIKey, local.UserAPIKey)
	}
	if !strings.HasPrefix(local.ConfigPath, filepath.Join(root, "secrets")) || !strings.HasPrefix(local.UserConfigPath, filepath.Join(root, "secrets")) {
		t.Fatalf("secret configs escaped secret dir: %s %s", local.ConfigPath, local.UserConfigPath)
	}
}

func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func runServiceHelper() {
	listen := ""
	configPath := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--listen=") {
			listen = strings.TrimPrefix(arg, "--listen=")
		}
	}
	for i, arg := range os.Args {
		if arg == "--host" && i+1 < len(os.Args) {
			listen = os.Args[i+1]
		}
		if arg == "--port" && i+1 < len(os.Args) {
			listen += ":" + os.Args[i+1]
		}
		if arg == "--config" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
		}
	}
	if listen == "" {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ready"}`)) })
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		accepted := key == "test-user" || key == "scoped-user-key"
		if configPath != "" {
			raw, _ := os.ReadFile(configPath)
			var config map[string]any
			_ = json.Unmarshal(raw, &config)
			server, _ := config["server"].(map[string]any)
			accepted = accepted || key == fmt.Sprint(server["root_api_key"])
		}
		if !accepted {
			_, _ = w.Write([]byte(`{"status":"ok","auth_mode":"api_key"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","auth_mode":"api_key","account_id":"a","user_id":"runner","role":"USER"}`))
	})
	mux.HandleFunc("/api/v1/admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		if configPath != "" {
			raw, _ := os.ReadFile(configPath)
			var config map[string]any
			_ = json.Unmarshal(raw, &config)
			server, _ := config["server"].(map[string]any)
			if r.Header.Get("X-API-Key") != fmt.Sprint(server["root_api_key"]) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		_, _ = w.Write([]byte(`{"result":{"user_key":"scoped-user-key"}}`))
	})
	if os.Getenv("OVTEST_SERVICE_EXIT") == "1" {
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(7)
		}()
	}
	if err := http.ListenAndServe(listen, mux); err != nil {
		os.Exit(3)
	}
}
