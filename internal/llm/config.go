package llm

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Enabled bool

	Provider string

	BaseURL string
	APIKey  string
	Model   string

	SystemPrompt string

	Timeout time.Duration

	MaxHistoryEvents   int
	Stream             bool
	MaxToolIters       int
	ToolOutputMaxBytes int

	// OpenAI-compatible
	ChatPath          string
	AuthHeader        string
	AuthValuePrefix   string
	APIKeyHeader      string
	APIKeyValuePrefix string

	// Gemini
	GeminiOperator  string
	GeminiKeyHeader string
}

func (c Config) ChatURL() (string, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"))
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(c.ChatPath)
	if p == "" {
		p = "/v1/chat/completions"
	}
	u := base.ResolveReference(&url.URL{Path: p})
	return u.String(), nil
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Provider) == "" {
		return fmt.Errorf("XR_LLM_PROVIDER is required when XR_LLM_ENABLED is true")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("XR_LLM_BASE_URL is required when XR_LLM_ENABLED is true")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("XR_LLM_MODEL is required when XR_LLM_ENABLED is true")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		// Some deployments may use upstream auth/sidecars, but keep explicit for now.
		return fmt.Errorf("XR_LLM_API_KEY is required when XR_LLM_ENABLED is true")
	}
	if err := ValidateOpenAICompatBaseURL(c.BaseURL, c.Provider, c.ChatPath); err != nil {
		return err
	}
	return nil
}

func ValidateOpenAICompatBaseURL(baseURL string, provider string, chatPath string) error {
	bu := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if bu == "" {
		return nil
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "", "openai_compat", "openai", "deepseek":
	default:
		return nil
	}
	cp := strings.TrimSpace(chatPath)
	if cp == "" {
		cp = "/v1/chat/completions"
	}
	if strings.HasSuffix(bu, "/v1") && strings.HasPrefix(cp, "/v1/") {
		return fmt.Errorf("XR_LLM_BASE_URL ends with /v1 while XR_LLM_CHAT_PATH is %q; this would call /v1/v1/... (set base URL to https://api.cometapi.com or set XR_LLM_CHAT_PATH=/chat/completions)", cp)
	}
	return nil
}

func FromEnv() Config {
	enabled := parseBoolEnv("XR_LLM_ENABLED", false) || parseBoolEnv("XR_LLM_ENABLE", false)
	// Default to a conservative, user-friendly timeout: many OpenAI-compatible gateways
	// (and tool-calling flows) can legitimately take >60s.
	timeout := parseDurationMillisEnv("XR_LLM_TIMEOUT_MS", 180_000)
	maxHist := parseIntEnv("XR_LLM_MAX_HISTORY_EVENTS", 50)
	stream := parseBoolEnv("XR_LLM_STREAM", true)
	maxIters := parseIntEnv("XR_LLM_MAX_TOOL_ITERS", 5)
	maxOut := parseIntEnv("XR_LLM_TOOL_OUTPUT_MAX_BYTES", 64*1024)

	// Compatibility with common agent env vars.
	// XR_LLM_* wins; fall back to OPENAI_* when unset.
	baseURL := strings.TrimSpace(os.Getenv("XR_LLM_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	}
	apiKey := strings.TrimSpace(os.Getenv("XR_LLM_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	model := strings.TrimSpace(os.Getenv("XR_LLM_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	}

	cfg := Config{
		Enabled:            enabled,
		Provider:           strings.TrimSpace(os.Getenv("XR_LLM_PROVIDER")),
		BaseURL:            baseURL,
		APIKey:             apiKey,
		Model:              model,
		SystemPrompt:       os.Getenv("XR_LLM_SYSTEM"),
		Timeout:            timeout,
		MaxHistoryEvents:   maxHist,
		Stream:             stream,
		MaxToolIters:       maxIters,
		ToolOutputMaxBytes: maxOut,

		ChatPath:          defaultEnv("XR_LLM_CHAT_PATH", "/v1/chat/completions"),
		AuthHeader:        defaultEnv("XR_LLM_AUTH_HEADER", "Authorization"),
		AuthValuePrefix:   defaultEnv("XR_LLM_AUTH_PREFIX", "Bearer "),
		APIKeyHeader:      strings.TrimSpace(os.Getenv("XR_LLM_API_KEY_HEADER")),
		APIKeyValuePrefix: defaultEnv("XR_LLM_API_KEY_PREFIX", ""),

		GeminiOperator:  defaultEnv("XR_LLM_GEMINI_OPERATOR", "generateContent"),
		GeminiKeyHeader: defaultEnv("XR_LLM_GEMINI_KEY_HEADER", "x-goog-api-key"),
	}
	if cfg.Provider == "" {
		cfg.Provider = "openai_compat"
	}
	return cfg
}

func defaultEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseBoolEnv(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func parseIntEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func parseDurationMillisEnv(key string, fallbackMillis int) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return time.Duration(fallbackMillis) * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return time.Duration(fallbackMillis) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}
