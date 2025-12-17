package llm

import "testing"

func TestValidateOpenAICompatBaseURL_DuplicateV1(t *testing.T) {
	err := ValidateOpenAICompatBaseURL("https://api.example.com/v1", "openai_compat", "/v1/chat/completions")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestValidateOpenAICompatBaseURL_OkWithoutV1(t *testing.T) {
	if err := ValidateOpenAICompatBaseURL("https://api.example.com", "openai_compat", "/v1/chat/completions"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateOpenAICompatBaseURL_OkWithNonV1Path(t *testing.T) {
	if err := ValidateOpenAICompatBaseURL("https://api.example.com/v1", "openai_compat", "/chat/completions"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

