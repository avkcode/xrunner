package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type streamingChatPrinter struct {
	out         io.Writer
	err         io.Writer
	interactive bool
	raw         bool
	mu          *sync.Mutex

	assistantActive bool
	assistantFull   string

	statusSpinning bool
	statusCancel   context.CancelFunc
	statusDetail   atomic.Value // string
	statusStart    time.Time
}

func newStreamingChatPrinter(out, err io.Writer, interactive bool, raw bool, mu *sync.Mutex) *streamingChatPrinter {
	return &streamingChatPrinter{
		out:         out,
		err:         err,
		interactive: interactive,
		raw:         raw,
		mu:          mu,
	}
}

func (p *streamingChatPrinter) Close() {
	if p == nil {
		return
	}
	p.stopThinkingSpinner(true)
	if p.assistantActive && p.interactive {
		p.lock()
		_, _ = fmt.Fprint(p.out, "\n")
		p.unlock()
		p.assistantActive = false
		p.assistantFull = ""
	}
}

// Handle prints an event and returns true when the caller should NOT print a prompt yet.
func (p *streamingChatPrinter) Handle(ev *xrunnerv1.AgentEvent) (suppressPrompt bool) {
	if ev == nil {
		return false
	}
	if p.raw {
		printChatEventTo(p.out, p.err, ev, true)
		return false
	}

	// Status UI: render "thinking" as an animated, single-line spinner.
	if st := ev.GetStatus(); st != nil && p.interactive {
		// Status is also a "non-chunk event", so end assistant streaming line first.
		if p.assistantActive {
			_, _ = fmt.Fprint(p.out, "\n")
			p.assistantActive = false
			p.assistantFull = ""
		}
		phase := strings.ToLower(strings.TrimSpace(st.GetPhase()))
		if phase == "thinking" {
			detail := strings.TrimSpace(st.GetDetail())
			p.startThinkingSpinner(detail)
			return true
		}
		// Any non-thinking status ends the spinner quietly (avoid noisy status lines).
		p.stopThinkingSpinner(true)
		return false
	}

	// When streaming assistant deltas arrive, render them on a single line in interactive terminals.
	if chunk := ev.GetToolOutputChunk(); chunk != nil {
		streamName := strings.ToLower(strings.TrimSpace(chunk.GetStream()))
		if streamName == "assistant" && p.interactive {
			p.stopThinkingSpinner(true)
			if !p.assistantActive {
				_, _ = fmt.Fprint(p.out, "\r\033[2K")
			}
			in := string(chunk.GetData())
			out := in
			if p.assistantFull != "" && strings.HasPrefix(in, p.assistantFull) {
				out = in[len(p.assistantFull):]
				p.assistantFull = in
			} else if p.assistantFull != "" && strings.HasPrefix(p.assistantFull, in) {
				out = ""
			} else {
				p.assistantFull += in
			}
			if !p.assistantActive {
				p.assistantActive = true
				_, _ = fmt.Fprint(p.out, "assistant: ")
			}
			if out != "" {
				_, _ = io.WriteString(p.out, out)
			}
			return true
		}
		// If we're mid-assistant stream and a different chunk arrives, end the assistant line first.
		if p.assistantActive && p.interactive {
			_, _ = fmt.Fprint(p.out, "\n")
			p.assistantActive = false
			p.assistantFull = ""
		}
		p.stopThinkingSpinner(true)
		if p.interactive {
			_, _ = fmt.Fprint(p.out, "\r\033[2K")
		}
		printChatEventTo(p.out, p.err, ev, false)
		return false
	}

	// Any non-chunk event ends the assistant streaming line.
	if p.assistantActive && p.interactive {
		_, _ = fmt.Fprint(p.out, "\n")
		p.assistantActive = false
		p.assistantFull = ""
	}
	p.stopThinkingSpinner(true)
	if p.interactive {
		_, _ = fmt.Fprint(p.out, "\r\033[2K")
	}

	printChatEventTo(p.out, p.err, ev, false)
	if p.interactive {
		// Avoid printing a new prompt immediately after the echoed user message,
		// otherwise the prompt races with assistant streaming and yields output like:
		//   xrunner> Hi
		if ev.GetUserMessage() != nil {
			return true
		}
	}
	return false
}

func (p *streamingChatPrinter) lock() {
	if p.mu != nil {
		p.mu.Lock()
	}
}

func (p *streamingChatPrinter) unlock() {
	if p.mu != nil {
		p.mu.Unlock()
	}
}

func (p *streamingChatPrinter) startThinkingSpinner(detail string) {
	if p.raw || !p.interactive {
		return
	}
	p.statusDetail.Store(strings.TrimSpace(detail))
	if p.statusSpinning {
		return
	}
	p.statusSpinning = true
	p.statusStart = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	p.statusCancel = cancel

	go func() {
		frames := []string{"|", "/", "-", "\\"}
		i := 0
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				elapsed := time.Since(p.statusStart).Truncate(100 * time.Millisecond)
				msg := "thinking"
				if v := p.statusDetail.Load(); v != nil {
					if d := strings.TrimSpace(v.(string)); d != "" {
						msg += " (" + d + ")"
					}
				}
				msg += " " + frames[i%len(frames)] + " " + elapsed.String()
				i++

				p.lock()
				_, _ = fmt.Fprint(p.err, "\r\033[2Kstatus: "+msg)
				p.unlock()
			}
		}
	}()
}

func (p *streamingChatPrinter) stopThinkingSpinner(clear bool) {
	if !p.statusSpinning {
		return
	}
	p.statusSpinning = false
	if p.statusCancel != nil {
		p.statusCancel()
		p.statusCancel = nil
	}
	if clear && p.interactive {
		p.lock()
		_, _ = fmt.Fprint(p.err, "\r\033[2K")
		p.unlock()
	}
}

func printChatEventTo(out, errw io.Writer, ev *xrunnerv1.AgentEvent, raw bool) {
	switch k := ev.GetKind().(type) {
	case *xrunnerv1.AgentEvent_AssistantMessage:
		if raw {
			return
		}
		txt := strings.TrimSpace(k.AssistantMessage.GetText())
		if txt != "" {
			_, _ = fmt.Fprintf(out, "assistant: %s\n", txt)
		}
	case *xrunnerv1.AgentEvent_UserMessage:
		if raw {
			return
		}
		txt := strings.TrimSpace(k.UserMessage.GetText())
		if txt != "" {
			_, _ = fmt.Fprintf(out, "you: %s\n", txt)
		}
	case *xrunnerv1.AgentEvent_ToolCallStarted:
		if raw {
			return
		}
		_, _ = fmt.Fprintf(out, "tool: %s (%s)\n", k.ToolCallStarted.GetName(), k.ToolCallStarted.GetToolCallId())
	case *xrunnerv1.AgentEvent_ToolOutputChunk:
		streamName := strings.ToLower(strings.TrimSpace(k.ToolOutputChunk.GetStream()))
		if streamName == "stderr" {
			_, _ = errw.Write(k.ToolOutputChunk.GetData())
			return
		}
		_, _ = out.Write(k.ToolOutputChunk.GetData())
	case *xrunnerv1.AgentEvent_ToolCallResult:
		if raw {
			return
		}
		exit := k.ToolCallResult.GetExitCode()
		msg := strings.TrimSpace(k.ToolCallResult.GetErrorMessage())
		if msg != "" {
			_, _ = fmt.Fprintf(out, "tool done: exit=%d err=%s\n", exit, msg)
		} else {
			_, _ = fmt.Fprintf(out, "tool done: exit=%d\n", exit)
		}
	case *xrunnerv1.AgentEvent_Status:
		if raw {
			return
		}
		// Status is handled by streamingChatPrinter for interactive terminals.
		// Keep a fallback for non-interactive callers.
		phase := k.Status.GetPhase()
		detail := strings.TrimSpace(k.Status.GetDetail())
		if detail != "" {
			_, _ = fmt.Fprintf(errw, "status: %s (%s)\n", phase, detail)
			return
		}
		_, _ = fmt.Fprintf(errw, "status: %s\n", phase)
	case *xrunnerv1.AgentEvent_Error:
		if raw {
			return
		}
		_, _ = fmt.Fprintf(errw, "error: %s\n", k.Error.GetMessage())
	default:
		if raw {
			return
		}
	}
}

func printChatEvent(ev *xrunnerv1.AgentEvent, raw bool) {
	printChatEventTo(os.Stdout, os.Stderr, ev, raw)
}
