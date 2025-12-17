package llm

import "context"

type ToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

type Message struct {
	Role string

	// For regular user/assistant messages.
	Content string

	// For assistant tool-call messages (OpenAI-compatible).
	ToolCalls []ToolCall

	// For tool response messages (OpenAI-compatible).
	ToolCallID string
}

type Request struct {
	Messages []Message

	// OpenAI-compatible tools (function calling). Gemini uses different schema and
	// currently ignores this field.
	Tools any

	// If set, called with incremental text deltas (best-effort; provider-dependent).
	OnTextDelta func(delta string)
}

type Result struct {
	Text      string
	ToolCalls []ToolCall
}

type Client interface {
	Generate(ctx context.Context, req Request) (Result, error)
}
