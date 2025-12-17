package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type openAICompatClient struct {
	http *http.Client
	cfg  Config
	u    *url.URL
}

func newOpenAICompatClient(httpClient *http.Client, cfg Config) (Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("XR_LLM_BASE_URL: %w", err)
	}
	return &openAICompatClient{http: httpClient, cfg: cfg, u: base}, nil
}

type oaiChatReq struct {
	Model      string       `json:"model"`
	Messages   []oaiChatMsg `json:"messages"`
	Stream     bool         `json:"stream,omitempty"`
	Tools      any          `json:"tools,omitempty"`
	ToolChoice any          `json:"tool_choice,omitempty"`
}

type oaiChatMsg struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type oaiChatResp struct {
	Choices []struct {
		Message struct {
			Role      string        `json:"role"`
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

type oaiStreamResp struct {
	Choices []struct {
		Delta struct {
			Content   string        `json:"content,omitempty"`
			ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
			// Legacy function_call support (some OpenAI-compat providers)
			FunctionCall *struct {
				Name      string `json:"name,omitempty"`
				Arguments string `json:"arguments,omitempty"`
			} `json:"function_call,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
		Index        int    `json:"index,omitempty"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (c *openAICompatClient) Generate(ctx context.Context, req Request) (Result, error) {
	reqURL := c.u.ResolveReference(&url.URL{Path: strings.TrimSpace(c.cfg.ChatPath)})

	msgs := make([]oaiChatMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "user"
		}
		out := oaiChatMsg{Role: role}
		if len(m.ToolCalls) > 0 {
			out.ToolCalls = make([]oaiToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				var call oaiToolCall
				call.ID = tc.ID
				call.Type = "function"
				call.Function.Name = tc.Name
				call.Function.Arguments = tc.ArgsJSON
				out.ToolCalls = append(out.ToolCalls, call)
			}
		}
		if strings.EqualFold(role, "tool") {
			out.ToolCallID = m.ToolCallID
			out.Content = m.Content
		} else {
			out.Content = m.Content
		}
		msgs = append(msgs, out)
	}

	payload := oaiChatReq{
		Model:    c.cfg.Model,
		Messages: msgs,
		Tools:    req.Tools,
	}
	if req.OnTextDelta != nil {
		payload.Stream = true
	} else {
		payload.Stream = false
	}

	// Encourage tool usage when tools are provided.
	if req.Tools != nil {
		payload.ToolChoice = "auto"
	}

	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// OpenAI-compatible providers universally accept this header format.
	// Keep it hard-coded to avoid subtle env misconfiguration (e.g. missing space in "Bearer ").
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return Result{}, fmt.Errorf("llm http %d %s: %s", resp.StatusCode, reqURL.String(), strings.TrimSpace(string(b)))
	}

	if req.OnTextDelta != nil {
		return c.readStream(resp.Body, req.OnTextDelta)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Result{}, err
	}
	var out oaiChatResp
	if err := json.Unmarshal(b, &out); err != nil {
		return Result{}, fmt.Errorf("llm decode: %w", err)
	}
	if out.Error != nil && strings.TrimSpace(out.Error.Message) != "" {
		return Result{}, fmt.Errorf("llm error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Result{}, fmt.Errorf("llm: empty choices")
	}
	msg := out.Choices[0].Message
	return Result{
		Text:      msg.Content,
		ToolCalls: toToolCalls(msg.ToolCalls),
	}, nil
}

func (c *openAICompatClient) readStream(r io.Reader, onDelta func(string)) (Result, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var text strings.Builder
	toolCalls := make(map[int]ToolCall) // index -> call

	// Minimal SSE parser: collect "data:" lines until a blank line, then emit one event.
	// Providers differ in whether each JSON chunk is single-line or split across multiple data lines.
	var dataLines []string
	flushEvent := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if data == "" {
			return false, nil
		}
		if data == "[DONE]" {
			return true, nil
		}
		var chunk oaiStreamResp
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Ignore malformed frames (some providers send keepalive garbage).
			return false, nil
		}
		if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
			return false, fmt.Errorf("llm error: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if d := choice.Delta.Content; d != "" {
				onDelta(d)
				text.WriteString(d)
			}
			if choice.Delta.FunctionCall != nil {
				// Legacy single function call: treat as tool call index 0.
				tc := toolCalls[0]
				if tc.ID == "" {
					tc.ID = "call_0"
				}
				if tc.Name == "" {
					tc.Name = choice.Delta.FunctionCall.Name
				}
				tc.ArgsJSON += choice.Delta.FunctionCall.Arguments
				toolCalls[0] = tc
			}
			for i, tcDelta := range choice.Delta.ToolCalls {
				// Some providers include tcDelta.index; others omit it. When omitted and multiple tool calls
				// are present, fall back to slice order.
				idx := tcDelta.Index
				if idx == 0 && len(choice.Delta.ToolCalls) > 1 {
					idx = i
				}
				tc := toolCalls[idx]
				if tc.ID == "" {
					tc.ID = tcDelta.ID
				}
				if tc.Name == "" {
					tc.Name = tcDelta.Function.Name
				}
				if tcDelta.Function.Arguments != "" {
					tc.ArgsJSON += tcDelta.Function.Arguments
				}
				toolCalls[idx] = tc
			}
		}
		return false, nil
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return Result{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			done, ferr := flushEvent()
			if ferr != nil {
				return Result{}, ferr
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		// Ignore other SSE fields (event:, id:, retry:).
	}
	if _, err := flushEvent(); err != nil {
		return Result{}, err
	}

	ordered := make([]ToolCall, 0, len(toolCalls))
	for i := 0; i < len(toolCalls); i++ {
		if tc, ok := toolCalls[i]; ok && (tc.Name != "" || tc.ArgsJSON != "") {
			ordered = append(ordered, tc)
		}
	}
	return Result{Text: text.String(), ToolCalls: ordered}, nil
}

func toToolCalls(calls []oaiToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{
			ID:       c.ID,
			Name:     c.Function.Name,
			ArgsJSON: c.Function.Arguments,
		})
	}
	return out
}
