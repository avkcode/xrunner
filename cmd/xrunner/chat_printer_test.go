package main

import (
	"strings"
	"sync"
	"testing"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

func TestStreamingChatPrinter_AssistantStreamSuppressesPrompt(t *testing.T) {
	var out, err strings.Builder
	p := newStreamingChatPrinter(&out, &err, true, false, &sync.Mutex{})

	suppress := p.Handle(&xrunnerv1.AgentEvent{
		Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
			ToolCallId: "assistant:1",
			Stream:     "assistant",
			Data:       []byte("hi"),
		}},
	})
	if !suppress {
		t.Fatalf("expected suppressPrompt")
	}
	if out.String() != "\r\033[2Kassistant: hi" {
		t.Fatalf("out=%q", out.String())
	}

	suppress = p.Handle(&xrunnerv1.AgentEvent{
		Kind: &xrunnerv1.AgentEvent_Status{Status: &xrunnerv1.Status{Phase: "idle"}},
	})
	if suppress {
		t.Fatalf("expected prompt allowed after non-chunk")
	}
	if out.String() != "\r\033[2Kassistant: hi\n" {
		t.Fatalf("out=%q", out.String())
	}
	if err.String() != "" {
		t.Fatalf("err=%q", err.String())
	}
}

func TestStreamingChatPrinter_RawDoesNotPrefixAssistant(t *testing.T) {
	var out, err strings.Builder
	p := newStreamingChatPrinter(&out, &err, true, true, &sync.Mutex{})
	_ = p.Handle(&xrunnerv1.AgentEvent{
		Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
			ToolCallId: "assistant:1",
			Stream:     "assistant",
			Data:       []byte("hi"),
		}},
	})
	if out.String() != "hi" {
		t.Fatalf("out=%q", out.String())
	}
}

func TestStreamingChatPrinter_CumulativeAssistantChunks(t *testing.T) {
	var out, err strings.Builder
	p := newStreamingChatPrinter(&out, &err, true, false, &sync.Mutex{})

	_ = p.Handle(&xrunnerv1.AgentEvent{
		Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
			ToolCallId: "assistant:1",
			Stream:     "assistant",
			Data:       []byte("hello"),
		}},
	})
	_ = p.Handle(&xrunnerv1.AgentEvent{
		Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
			ToolCallId: "assistant:1",
			Stream:     "assistant",
			Data:       []byte("hello world"),
		}},
	})
	if out.String() != "\r\033[2Kassistant: hello world" {
		t.Fatalf("out=%q", out.String())
	}
}

func TestStreamingChatPrinter_UserMessageSuppressesPrompt(t *testing.T) {
	var out, err strings.Builder
	p := newStreamingChatPrinter(&out, &err, true, false, &sync.Mutex{})

	suppress := p.Handle(&xrunnerv1.AgentEvent{
		Kind: &xrunnerv1.AgentEvent_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: "hello"}},
	})
	if !suppress {
		t.Fatalf("expected suppressPrompt for echoed user message")
	}
	if out.String() != "\r\033[2Kyou: hello\n" {
		t.Fatalf("out=%q", out.String())
	}
	if err.String() != "" {
		t.Fatalf("err=%q", err.String())
	}
}
