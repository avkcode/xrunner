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

type geminiClient struct {
	http *http.Client
	cfg  Config
	u    *url.URL
}

func newGeminiClient(httpClient *http.Client, cfg Config) (Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("XR_LLM_BASE_URL: %w", err)
	}
	return &geminiClient{http: httpClient, cfg: cfg, u: base}, nil
}

type geminiReq struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
}

type geminiResp struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
}

func (c *geminiClient) Generate(ctx context.Context, req Request) (Result, error) {
	operator := strings.TrimSpace(c.cfg.GeminiOperator)
	if operator == "" {
		operator = "generateContent"
	}
	rel := &url.URL{Path: "/v1beta/models/" + url.PathEscape(c.cfg.Model) + ":" + operator}
	if req.OnTextDelta != nil && operator == "generateContent" {
		// Best-effort: if operator isn't explicitly set to streamGenerateContent,
		// use it for streaming.
		rel = &url.URL{
			Path:     "/v1beta/models/" + url.PathEscape(c.cfg.Model) + ":streamGenerateContent",
			RawQuery: "alt=sse",
		}
	}
	reqURL := c.u.ResolveReference(rel)

	contents := make([]geminiContent, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "assistant", "model":
			role = "model"
		case "user":
			role = "user"
		default:
			role = "user"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	body, _ := json.Marshal(geminiReq{Contents: contents})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(c.cfg.GeminiKeyHeader, c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return Result{}, fmt.Errorf("llm http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	if req.OnTextDelta != nil && reqURL.Query().Get("alt") == "sse" {
		return readGeminiSSE(resp.Body, req.OnTextDelta)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Result{}, err
	}
	var out geminiResp
	if err := json.Unmarshal(b, &out); err != nil {
		return Result{}, fmt.Errorf("llm decode: %w", err)
	}
	if len(out.Candidates) == 0 {
		return Result{}, fmt.Errorf("llm: empty candidates")
	}
	var sb strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return Result{Text: sb.String()}, nil
}

func readGeminiSSE(r io.Reader, onDelta func(string)) (Result, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var text strings.Builder
	lastLen := 0

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var out geminiResp
		if err := json.Unmarshal([]byte(data), &out); err != nil {
			continue
		}
		if len(out.Candidates) == 0 {
			continue
		}
		var sb strings.Builder
		for _, p := range out.Candidates[0].Content.Parts {
			sb.WriteString(p.Text)
		}
		full := sb.String()
		if len(full) > lastLen {
			delta := full[lastLen:]
			onDelta(delta)
			text.WriteString(delta)
			lastLen = len(full)
		}
	}
	if err := sc.Err(); err != nil {
		return Result{}, err
	}
	return Result{Text: text.String()}, nil
}
