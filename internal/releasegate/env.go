package releasegate

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const (
	ProtocolOpenAICompletions = "openai-completions"
	ProtocolOpenAIResponses   = "openai-responses"
)

type ModelCredentials struct {
	LLMAPIKey        string
	LLMBaseURL       string
	LLMModel         string
	LLMProtocol      string
	EmbeddingAPIKey  string
	EmbeddingBaseURL string
	EmbeddingModel   string
}

type OpenVikingCredentials struct {
	UserAPIKey string
}

type RuntimeCredentials struct {
	Model            ModelCredentials
	HarnessLLMAPIKey string
	OpenViking       OpenVikingCredentials
	Warnings         []string
}

var allowedEnvKeys = map[string]bool{
	"OPENVIKING_LLM_API_KEY":        true,
	"OPENVIKING_LLM_BASE_URL":       true,
	"OPENVIKING_LLM_MODEL":          true,
	"OPENVIKING_EMBEDDING_API_KEY":  true,
	"OPENVIKING_EMBEDDING_BASE_URL": true,
	"OPENVIKING_EMBEDDING_MODEL":    true,
	"OPENVIKING_API_KEY":            true,
	"OV_TEST_HARNESS_LLM_API_KEY":   true,
	"OV_TEST_OPENVIKING_API_KEY":    true,
	// Backward-compatible provider names used by the current local setup.
	"ARK_API_KEY":  true,
	"ARK_BASE_URL": true,
	"ARK_MODEL":    true,
}

var providerEnvKeys = map[string]bool{
	"OPENVIKING_LLM_API_KEY": true, "OPENVIKING_LLM_BASE_URL": true, "OPENVIKING_LLM_MODEL": true,
	"OPENVIKING_EMBEDDING_API_KEY": true, "OPENVIKING_EMBEDDING_BASE_URL": true, "OPENVIKING_EMBEDDING_MODEL": true,
	"OPENVIKING_API_KEY": true, "OV_TEST_HARNESS_LLM_API_KEY": true,
	"ARK_API_KEY": true, "ARK_BASE_URL": true, "ARK_MODEL": true,
}

func LoadEnvFile(path string) (RuntimeCredentials, error) {
	return loadEnvFile(path, nil)
}

// LoadEnvFileWithOverrides applies only the same whitelisted keys from the
// supplied process environment after parsing the file. This supports split
// provider credentials without importing unrelated shell secrets.
func LoadEnvFileWithOverrides(path string, environ []string) (RuntimeCredentials, error) {
	return loadEnvFile(path, environ)
}

func loadEnvFile(path string, environ []string) (RuntimeCredentials, error) {
	if path == "" {
		return RuntimeCredentials{}, fmt.Errorf("env file path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return RuntimeCredentials{}, err
	}
	var warnings []string
	if info.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("env file %s is accessible by group or others; chmod 600 is recommended", path))
	}
	f, err := os.Open(path)
	if err != nil {
		return RuntimeCredentials{}, err
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return RuntimeCredentials{}, fmt.Errorf("env file %s:%d: expected KEY=VALUE", path, lineNo)
		}
		if !allowedEnvKeys[key] {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return RuntimeCredentials{}, err
	}
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if ok && allowedEnvKeys[key] && value != "" {
			values[key] = value
		}
	}
	first := func(keys ...string) string {
		for _, key := range keys {
			if values[key] != "" {
				return values[key]
			}
		}
		return ""
	}
	llmBaseURL, llmProtocol := normalizeLLMEndpoint(first("OPENVIKING_LLM_BASE_URL", "ARK_BASE_URL"))
	embeddingBaseURL := normalizeEmbeddingEndpoint(values["OPENVIKING_EMBEDDING_BASE_URL"])
	model := ModelCredentials{
		LLMAPIKey:        first("OPENVIKING_LLM_API_KEY", "OPENVIKING_API_KEY", "ARK_API_KEY"),
		LLMBaseURL:       llmBaseURL,
		LLMModel:         first("OPENVIKING_LLM_MODEL", "ARK_MODEL"),
		LLMProtocol:      llmProtocol,
		EmbeddingAPIKey:  first("OPENVIKING_EMBEDDING_API_KEY", "OPENVIKING_API_KEY"),
		EmbeddingBaseURL: embeddingBaseURL,
		EmbeddingModel:   values["OPENVIKING_EMBEDDING_MODEL"],
	}
	return RuntimeCredentials{
		Model:            model,
		HarnessLLMAPIKey: first("OV_TEST_HARNESS_LLM_API_KEY", "OPENVIKING_LLM_API_KEY", "OPENVIKING_API_KEY", "ARK_API_KEY"),
		OpenViking:       OpenVikingCredentials{UserAPIKey: values["OV_TEST_OPENVIKING_API_KEY"]},
		Warnings:         warnings,
	}, nil
}

func (c RuntimeCredentials) OpenVikingProcess() map[string]string {
	return c.modelProcess()
}

// OpenVikingCaseProcess exposes the same typed model credentials to direct
// OpenViking cases and their optional semantic judge. The release runner still
// scrubs all ambient provider credentials before applying these scoped values.
func (c RuntimeCredentials) OpenVikingCaseProcess() map[string]string {
	out := c.modelProcess()
	put := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	put("ARK_API_KEY", c.Model.LLMAPIKey)
	put("ARK_BASE_URL", c.Model.LLMBaseURL)
	put("ARK_MODEL", c.Model.LLMModel)
	return out
}

func (c RuntimeCredentials) OpenClawProcess() map[string]string {
	out := map[string]string{}
	put := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	put("OPENVIKING_LLM_API_KEY", c.HarnessLLMAPIKey)
	put("OPENVIKING_LLM_BASE_URL", c.Model.LLMBaseURL)
	put("OPENVIKING_LLM_MODEL", c.Model.LLMModel)
	if c.Model.LLMProtocol != "" {
		out["OV_TEST_OPENCLAW_LLM_API"] = c.Model.LLMProtocol
	}
	if c.OpenViking.UserAPIKey != "" {
		out["OV_TEST_OPENCLAW_OPENVIKING_API_KEY"] = c.OpenViking.UserAPIKey
	}
	return out
}

func normalizeLLMEndpoint(raw string) (string, string) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, suffix := range []string{"/responses", "/chat/completions"} {
		if strings.HasSuffix(value, suffix) {
			protocol := ProtocolOpenAICompletions
			if suffix == "/responses" {
				protocol = ProtocolOpenAIResponses
			}
			return strings.TrimSuffix(value, suffix), protocol
		}
	}
	return value, ProtocolOpenAICompletions
}

func normalizeEmbeddingEndpoint(raw string) string {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, suffix := range []string{"/embeddings/multimodal", "/embeddings"} {
		if strings.HasSuffix(value, suffix) {
			return strings.TrimSuffix(value, suffix)
		}
	}
	return value
}
func (c RuntimeCredentials) HermesProcess() map[string]string {
	out := map[string]string{}
	if c.HarnessLLMAPIKey != "" {
		out["OPENAI_API_KEY"] = c.HarnessLLMAPIKey
		// Hermes intentionally host-gates generic OPENAI_API_KEY forwarding.
		// The generated named custom provider references this scoped variable so
		// authenticated non-OpenAI-compatible hosts receive the intended key.
		out["OV_TEST_HERMES_LLM_API_KEY"] = c.HarnessLLMAPIKey
	}
	if c.OpenViking.UserAPIKey != "" {
		out["OV_TEST_HERMES_OPENVIKING_API_KEY"] = c.OpenViking.UserAPIKey
	}
	return out
}

// Codex, Claude Code, and OpenCode use operator-managed machine auth. Provider
// credentials from the ovtest env file must never be inherited by them.
func (c RuntimeCredentials) CodexProcess() map[string]string {
	return scopedKey("OV_TEST_CODEX_OPENVIKING_API_KEY", c.OpenViking.UserAPIKey)
}
func (c RuntimeCredentials) ClaudeProcess() map[string]string {
	return scopedKey("OV_TEST_CLAUDE_OPENVIKING_API_KEY", c.OpenViking.UserAPIKey)
}
func (c RuntimeCredentials) OpenCodeProcess() map[string]string {
	return scopedKey("OV_TEST_OPENCODE_OPENVIKING_API_KEY", c.OpenViking.UserAPIKey)
}

func (c RuntimeCredentials) PiProcess() map[string]string {
	out := map[string]string{}
	put := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	put("OV_TEST_PI_LLM_API_KEY", c.HarnessLLMAPIKey)
	put("OV_TEST_PI_LLM_BASE_URL", c.Model.LLMBaseURL)
	put("OV_TEST_PI_MODEL", c.Model.LLMModel)
	put("OV_TEST_PI_LLM_PROTOCOL", c.Model.LLMProtocol)
	put("OV_TEST_PI_OPENVIKING_API_KEY", c.OpenViking.UserAPIKey)
	return out
}

func scopedKey(name, value string) map[string]string {
	if value == "" {
		return map[string]string{}
	}
	return map[string]string{name: value}
}

func (c RuntimeCredentials) modelProcess() map[string]string {
	out := map[string]string{}
	put := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	put("OPENVIKING_LLM_API_KEY", c.Model.LLMAPIKey)
	put("OPENVIKING_LLM_BASE_URL", c.Model.LLMBaseURL)
	put("OPENVIKING_LLM_MODEL", c.Model.LLMModel)
	put("OPENVIKING_EMBEDDING_API_KEY", c.Model.EmbeddingAPIKey)
	put("OPENVIKING_EMBEDDING_BASE_URL", c.Model.EmbeddingBaseURL)
	put("OPENVIKING_EMBEDDING_MODEL", c.Model.EmbeddingModel)
	return out
}

func ScrubProviderEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		upper := strings.ToUpper(key)
		// This names an isolated directory; it is not a credential. Keeping it
		// through harness-specific sanitization is what prevents generated auth
		// copies and OpenViking keys from landing in retained runtime evidence.
		if upper == "OV_TEST_SECRET_STATE_DIR" {
			out[key] = value
			continue
		}
		if providerEnvKeys[key] || ((strings.Contains(upper, "API_KEY") || strings.Contains(upper, "TOKEN") ||
			strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD")) && !isScopedOpenVikingOverride(upper)) {
			continue
		}
		out[key] = value
	}
	return out
}

func isScopedOpenVikingOverride(key string) bool {
	return strings.HasPrefix(key, "OV_TEST_") && strings.HasSuffix(key, "_OPENVIKING_API_KEY")
}
