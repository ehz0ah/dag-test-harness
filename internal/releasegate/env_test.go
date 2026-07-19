package releasegate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvSeparatesModelAndOpenVikingKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\nOPENVIKING_LLM_API_KEY=model-secret\nOPENVIKING_LLM_BASE_URL=https://llm.example/v1\nOPENVIKING_LLM_MODEL=model-x\nOPENVIKING_EMBEDDING_API_KEY=embed-secret\nOPENVIKING_EMBEDDING_BASE_URL=https://embed.example/v1\nOPENVIKING_EMBEDDING_MODEL=embed-x\nOV_TEST_HARNESS_LLM_API_KEY=harness-secret\nOV_TEST_OPENVIKING_API_KEY=ov-user-secret\nIGNORED=never-forward\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if env.Model.LLMAPIKey != "model-secret" || env.HarnessLLMAPIKey != "harness-secret" || env.OpenViking.UserAPIKey != "ov-user-secret" {
		t.Fatalf("typed env = %+v", env)
	}
	if _, ok := env.OpenVikingProcess()["IGNORED"]; ok {
		t.Fatal("unknown env leaked")
	}
	for _, child := range []map[string]string{env.CodexProcess(), env.ClaudeProcess(), env.OpenCodeProcess()} {
		for _, key := range []string{"OPENVIKING_LLM_API_KEY", "OPENVIKING_EMBEDDING_API_KEY", "OPENVIKING_API_KEY"} {
			if _, ok := child[key]; ok {
				t.Fatalf("%s leaked to authenticated harness: %v", key, child)
			}
		}
	}
	if env.CodexProcess()["OV_TEST_CODEX_OPENVIKING_API_KEY"] != "ov-user-secret" ||
		env.ClaudeProcess()["OV_TEST_CLAUDE_OPENVIKING_API_KEY"] != "ov-user-secret" ||
		env.OpenCodeProcess()["OV_TEST_OPENCODE_OPENVIKING_API_KEY"] != "ov-user-secret" {
		t.Fatal("scoped OpenViking key was not forwarded to authenticated harnesses")
	}
	if env.HermesProcess()["OPENAI_API_KEY"] != "harness-secret" ||
		env.HermesProcess()["OV_TEST_HERMES_LLM_API_KEY"] != "harness-secret" ||
		env.HermesProcess()["OV_TEST_HERMES_OPENVIKING_API_KEY"] != "ov-user-secret" {
		t.Fatalf("Hermes credential mapping = %v", env.HermesProcess())
	}
	if env.OpenClawProcess()["OV_TEST_OPENCLAW_OPENVIKING_API_KEY"] != "ov-user-secret" {
		t.Fatalf("OpenClaw credential mapping = %v", env.OpenClawProcess())
	}
	if env.OpenClawProcess()["OPENVIKING_LLM_API_KEY"] != "harness-secret" || env.PiProcess()["OV_TEST_PI_LLM_API_KEY"] != "harness-secret" {
		t.Fatal("shared harness key was not forwarded to OpenClaw and Pi")
	}
	if _, ok := env.OpenClawProcess()["OPENVIKING_EMBEDDING_API_KEY"]; ok {
		t.Fatal("OpenViking embedding key leaked to OpenClaw")
	}
	judge := env.OpenVikingCaseProcess()
	if judge["ARK_API_KEY"] != "model-secret" || judge["ARK_BASE_URL"] != "https://llm.example/v1" || judge["ARK_MODEL"] != "model-x" {
		t.Fatalf("OpenViking case judge mapping = %v", judge)
	}
}

func TestLoadEnvNormalizesEndpointURLsAndPreservesProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "OPENVIKING_LLM_BASE_URL=https://ark.example/api/v3/responses\n" +
		"OPENVIKING_EMBEDDING_BASE_URL=https://ark.example/api/v3/embeddings/multimodal\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if env.Model.LLMBaseURL != "https://ark.example/api/v3" ||
		env.Model.EmbeddingBaseURL != "https://ark.example/api/v3" {
		t.Fatalf("normalized model endpoints = %+v", env.Model)
	}
	if env.Model.LLMProtocol != ProtocolOpenAIResponses {
		t.Fatalf("LLM protocol = %q", env.Model.LLMProtocol)
	}
	if got := env.OpenClawProcess()["OV_TEST_OPENCLAW_LLM_API"]; got != ProtocolOpenAIResponses {
		t.Fatalf("OpenClaw API = %q", got)
	}
}

func TestLoadEnvDefaultsRootEndpointToChatCompletions(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OPENVIKING_LLM_BASE_URL=https://llm.example/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if env.Model.LLMBaseURL != "https://llm.example/v1" || env.Model.LLMProtocol != ProtocolOpenAICompletions {
		t.Fatalf("root endpoint mapping = %+v", env.Model)
	}
}

func TestLoadEnvRejectsMalformedLineAndLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("BROKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnvFile(path); err == nil {
		t.Fatal("malformed env accepted")
	}
	if err := os.WriteFile(path, []byte("OPENVIKING_API_KEY=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Warnings) != 1 {
		t.Fatalf("loose permissions warning = %v", env.Warnings)
	}
}

func TestLegacyOpenVikingAPIKeyIsProviderOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OPENVIKING_API_KEY=legacy-model-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if env.Model.LLMAPIKey != "legacy-model-secret" || env.Model.EmbeddingAPIKey != "legacy-model-secret" {
		t.Fatalf("legacy provider mapping = %+v", env.Model)
	}
	if env.OpenViking.UserAPIKey != "" {
		t.Fatalf("legacy provider key became server user key: %+v", env.OpenViking)
	}
	if env.HarnessLLMAPIKey != "legacy-model-secret" {
		t.Fatalf("legacy provider key did not remain the harness fallback: %+v", env)
	}
}

func TestLoadEnvFileWithOverridesUsesOnlyWhitelistedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OPENVIKING_API_KEY=legacy\nOPENVIKING_LLM_MODEL=file-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFileWithOverrides(path, []string{
		"OPENVIKING_LLM_API_KEY=split-llm", "OPENVIKING_EMBEDDING_API_KEY=split-embedding",
		"OPENVIKING_LLM_MODEL=process-model", "UNRELATED_SECRET=never-import",
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.Model.LLMAPIKey != "split-llm" || env.Model.EmbeddingAPIKey != "split-embedding" || env.Model.LLMModel != "process-model" {
		t.Fatalf("overridden credentials = %+v", env.Model)
	}
	if _, ok := env.OpenVikingProcess()["UNRELATED_SECRET"]; ok {
		t.Fatal("unrelated process secret was imported")
	}
}

func TestScrubProviderEnvKeepsOnlyScopedOpenVikingOverride(t *testing.T) {
	got := ScrubProviderEnv(map[string]string{
		"PATH": "/bin", "OPENAI_API_KEY": "provider", "SESSION_TOKEN": "provider",
		"OV_TEST_CODEX_OPENVIKING_API_KEY": "scoped",
		"OV_TEST_SECRET_STATE_DIR":         "/isolated/secrets",
	})
	if got["PATH"] != "/bin" || got["OV_TEST_CODEX_OPENVIKING_API_KEY"] != "scoped" ||
		got["OV_TEST_SECRET_STATE_DIR"] != "/isolated/secrets" || got["OPENAI_API_KEY"] != "" || got["SESSION_TOKEN"] != "" {
		t.Fatalf("scrubbed env = %v", got)
	}
}
