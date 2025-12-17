package llm

import (
	"fmt"
	"net/http"
	"strings"
)

func NewClient(cfg Config) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}

	httpClient := &http.Client{Timeout: cfg.Timeout}

	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "openai_compat", "openai", "deepseek":
		return newOpenAICompatClient(httpClient, cfg)
	case "gemini":
		return newGeminiClient(httpClient, cfg)
	default:
		return nil, fmt.Errorf("unsupported XR_LLM_PROVIDER %q", cfg.Provider)
	}
}
