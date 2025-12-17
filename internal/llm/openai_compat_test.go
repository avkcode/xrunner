package llm

import (
	"strings"
	"testing"
)

func TestOpenAICompat_ReadStream_SSEMultiLineData(t *testing.T) {
	c := &openAICompatClient{}

	// One SSE event split across multiple data lines (newline is legal JSON whitespace).
	sse := strings.Join([]string{
		"data: {\"choices\":",
		"data: [{\"delta\":{\"content\":\"hi\"},\"index\":0}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var got strings.Builder
	res, err := c.readStream(strings.NewReader(sse), func(d string) { got.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "hi" {
		t.Fatalf("delta=%q", got.String())
	}
	if res.Text != "hi" {
		t.Fatalf("text=%q", res.Text)
	}
}

func TestOpenAICompat_ReadStream_ToolCallIndexFallback(t *testing.T) {
	c := &openAICompatClient{}

	// Two tool calls in one frame with no explicit "index" field: we should fall back to slice order.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[` +
			`{"id":"call_0","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"echo a\"}"}}` +
			`,` +
			`{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"echo b\"}"}}` +
			`]}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	res, err := c.readStream(strings.NewReader(sse), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("tool_calls=%d", len(res.ToolCalls))
	}
	if res.ToolCalls[0].ID != "call_0" || !strings.Contains(res.ToolCalls[0].ArgsJSON, "echo a") {
		t.Fatalf("call0=%+v", res.ToolCalls[0])
	}
	if res.ToolCalls[1].ID != "call_1" || !strings.Contains(res.ToolCalls[1].ArgsJSON, "echo b") {
		t.Fatalf("call1=%+v", res.ToolCalls[1])
	}
}
