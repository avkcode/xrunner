package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type eventMsg struct {
	ev *xrunnerv1.AgentEvent
}

type errMsg struct {
	err error
}

type model struct {
	ctx    context.Context
	cancel context.CancelFunc

	conn   *grpc.ClientConn
	svc    xrunnerv1.SessionServiceClient
	stream xrunnerv1.SessionService_InteractClient

	sessionID string

	viewport viewport.Model
	composer textarea.Model
	status   string

	pendingTool    strings.Builder
	flushScheduled bool
	flushEvery     time.Duration

	assistantActive bool
	assistantLive   strings.Builder
	assistantFull   string

	scrollback *scrollback

	paletteOpen bool
	palette     list.Model
}

type textMsg struct {
	text string
}

type flushMsg struct{}

type paletteItem struct {
	title string
	desc  string

	insert string
	action func(*model) tea.Cmd
}

func (i paletteItem) Title() string       { return i.title }
func (i paletteItem) Description() string { return i.desc }
func (i paletteItem) FilterValue() string { return i.title + " " + i.desc }

type scrollback struct {
	lines        []string
	bytes        int
	maxLines     int
	maxBytes     int
	droppedLines int
	droppedBytes int
}

func newScrollback(maxLines, maxBytes int) *scrollback {
	return &scrollback{maxLines: maxLines, maxBytes: maxBytes}
}

func (s *scrollback) Append(text string) {
	if text == "" {
		return
	}
	parts := strings.SplitAfter(text, "\n")
	for _, p := range parts {
		if p == "" {
			continue
		}
		s.lines = append(s.lines, p)
		s.bytes += len(p)
	}
	s.trim()
}

func (s *scrollback) trim() {
	for (s.maxLines > 0 && len(s.lines) > s.maxLines) || (s.maxBytes > 0 && s.bytes > s.maxBytes) {
		if len(s.lines) == 0 {
			s.bytes = 0
			return
		}
		d := s.lines[0]
		s.lines = s.lines[1:]
		s.bytes -= len(d)
		if s.bytes < 0 {
			s.bytes = 0
		}
		s.droppedLines++
		s.droppedBytes += len(d)
	}
}

func (s *scrollback) Compact(keepLines int) {
	if keepLines <= 0 || keepLines >= len(s.lines) {
		return
	}
	drop := len(s.lines) - keepLines
	for i := 0; i < drop; i++ {
		s.droppedLines++
		s.droppedBytes += len(s.lines[i])
	}
	s.lines = append([]string(nil), s.lines[drop:]...)
	s.bytes = 0
	for _, l := range s.lines {
		s.bytes += len(l)
	}
	s.trim()
}

func (s *scrollback) Content() string {
	if len(s.lines) == 0 && s.droppedLines == 0 {
		return ""
	}
	var b strings.Builder
	if s.droppedLines > 0 {
		fmt.Fprintf(&b, "[compact] dropped %d lines (%d bytes)\n", s.droppedLines, s.droppedBytes)
	}
	for _, l := range s.lines {
		b.WriteString(l)
	}
	return b.String()
}

func (s *scrollback) Stats() (keptLines, keptBytes, droppedLines, droppedBytes int) {
	return len(s.lines), s.bytes, s.droppedLines, s.droppedBytes
}

func main() {
	var addr string
	var workspace string
	var shellName string
	var toolBundle string
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.StringVar(&addr, "addr", "127.0.0.1:7337", "xrunner-remote address")
	flag.StringVar(&workspace, "workspace", "/", "remote workspace root")
	flag.StringVar(&shellName, "shell", "", "attach to durable shell session name (tmux-backed)")
	flag.StringVar(&toolBundle, "tool-bundle", "", "path to OpenAI-compatible tool bundle JSON (default $AGENT_TOOL_SPEC)")

	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(0)
	}
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()

	if strings.TrimSpace(shellName) != "" {
		runShellTUI(ctx, conn, strings.TrimSpace(shellName))
		return
	}

	svc := xrunnerv1.NewSessionServiceClient(conn)
	if strings.TrimSpace(toolBundle) == "" {
		toolBundle = strings.TrimSpace(os.Getenv("AGENT_TOOL_SPEC"))
	}
	created, err := svc.CreateSession(ctx, &xrunnerv1.CreateSessionRequest{
		WorkspaceRoot:  workspace,
		ToolBundlePath: toolBundle,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create session:", err)
		os.Exit(1)
	}

	stream, err := svc.Interact(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "interact:", err)
		os.Exit(1)
	}
	if err := stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_Hello{Hello: &xrunnerv1.ClientHello{
		SessionId:    created.GetSession().GetSessionId(),
		AfterEventId: 0,
	}}}); err != nil {
		fmt.Fprintln(os.Stderr, "hello:", err)
		os.Exit(1)
	}

	ta := textarea.New()
	ta.Placeholder = "Type a message… (Ctrl+S send • Ctrl+P palette)"
	ta.Focus()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = "> "
	ta.SetHeight(5)
	ta.SetWidth(80)

	vp := viewport.New(0, 0)
	vp.SetContent("")

	pal := list.New([]list.Item{
		paletteItem{title: "/shell", desc: "run a remote shell tool", insert: "/shell "},
		paletteItem{title: "/write", desc: "write a file: /write <path> <content>", insert: "/write "},
		paletteItem{title: "/sessions", desc: "list sessions (local)", insert: "/sessions"},
		paletteItem{title: "/compact", desc: "compact scrollback (local)", action: func(m *model) tea.Cmd {
			keep := 500
			if m.scrollback != nil && m.scrollback.maxLines > 0 {
				keep = minInt(keep, m.scrollback.maxLines)
			}
			m.scrollback.Compact(keep)
			m.renderScrollback()
			return nil
		}},
		paletteItem{title: "/help", desc: "show keybindings", action: func(m *model) tea.Cmd {
			m.append("[help] Ctrl+S send | Ctrl+P palette | Esc close palette/quit | /compact | /shell | /write | /sessions\n")
			return nil
		}},
	}, list.NewDefaultDelegate(), 0, 0)
	pal.SetShowHelp(false)
	pal.Title = "Command palette"

	m := model{
		ctx:        ctx,
		cancel:     cancel,
		conn:       conn,
		svc:        svc,
		stream:     stream,
		sessionID:  created.GetSession().GetSessionId(),
		viewport:   vp,
		composer:   ta,
		status:     "connected",
		flushEvery: 50 * time.Millisecond,
		scrollback: newScrollback(5000, 1<<20),
		palette:    pal,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runShellTUI(ctx context.Context, conn *grpc.ClientConn, name string) {
	shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
	_, err := shellc.EnsureShellSession(ctx, &xrunnerv1.EnsureShellSessionRequest{Name: name})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ensure shell:", err)
		os.Exit(1)
	}
	stream, err := shellc.AttachShell(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "attach shell:", err)
		os.Exit(1)
	}
	if err := stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
		Name:              name,
		AfterOffset:       0,
		ClientId:          "tui",
		ResumeFromLastAck: true,
		MaxChunkBytes:     0,
		PollMs:            0,
	}}}); err != nil {
		fmt.Fprintln(os.Stderr, "shell hello:", err)
		os.Exit(1)
	}

	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	ta := textarea.New()
	ta.Placeholder = "type a command (Ctrl+S to send)"
	ta.Focus()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = "> "
	ta.SetHeight(3)
	ta.SetWidth(80)

	vp := viewport.New(0, 0)
	vp.SetContent("")

	m := shellModel{
		ctx:      ctx2,
		cancel:   cancel,
		stream:   stream,
		name:     name,
		clientID: "tui",
		viewport: vp,
		input:    ta,
		status:   "attached",
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx2))
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type shellChunkMsg struct {
	offset          uint64
	data            []byte
	uncompressedLen uint32
}

type shellModel struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stream   xrunnerv1.RemoteShellService_AttachShellClient
	name     string
	clientID string

	viewport viewport.Model
	input    textarea.Model

	content string
	status  string
	lastAck uint64
}

func (m shellModel) Init() tea.Cmd {
	return tea.Batch(m.recvShellCmd(), tea.EnterAltScreen)
}

func (m shellModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = t.Width
		m.input.SetWidth(t.Width - 2)
		m.viewport.Height = t.Height - 1 - m.input.Height()
		if m.viewport.Height < 1 {
			m.viewport.Height = 1
		}
		m.viewport.YPosition = 0
		return m, nil
	case tea.KeyMsg:
		switch t.String() {
		case "ctrl+c", "esc":
			m.cancel()
			_ = m.stream.CloseSend()
			return m, tea.Quit
		case "ctrl+s", "alt+enter":
			line := strings.TrimSpace(m.input.Value())
			line = strings.ReplaceAll(line, "\r", " ")
			line = strings.ReplaceAll(line, "\n", " ")
			m.input.SetValue("")
			if line == "" {
				return m, nil
			}
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			return m, tea.Batch(m.sendShellLineCmd(line), m.recvShellCmd())
		}
	case shellChunkMsg:
		if len(t.data) > 0 {
			m.append(string(t.data))
			ack := t.offset + uint64(t.uncompressedLen)
			if ack > m.lastAck {
				m.lastAck = ack
				return m, tea.Batch(m.ackShellCmd(ack), m.recvShellCmd())
			}
		}
		return m, m.recvShellCmd()
	case errMsg:
		if errors.Is(t.err, io.EOF) {
			m.status = "disconnected"
			m.append("\n[disconnected]\n")
			return m, nil
		}
		m.status = "error: " + t.err.Error()
		m.append("\n" + m.status + "\n")
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m shellModel) View() string {
	header := fmt.Sprintf("xrunner shell %s | %s\n", m.name, m.status)
	return header + m.viewport.View() + "\n" + m.input.View()
}

func (m *shellModel) append(s string) {
	m.content += s
	m.viewport.SetContent(m.content)
	m.viewport.GotoBottom()
}

func (m shellModel) recvShellCmd() tea.Cmd {
	return func() tea.Msg {
		msg, err := m.stream.Recv()
		if err != nil {
			return errMsg{err: err}
		}
		if chunk := msg.GetChunk(); chunk != nil && len(chunk.GetData()) > 0 {
			u := chunk.GetUncompressedLen()
			if u == 0 {
				u = uint32(len(chunk.GetData()))
			}
			return shellChunkMsg{offset: chunk.GetOffset(), data: chunk.GetData(), uncompressedLen: u}
		}
		return nil
	}
}

func (m shellModel) sendShellLineCmd(line string) tea.Cmd {
	return func() tea.Msg {
		_ = m.stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
			Name: m.name,
			// Keep as a single-line command; shell handles it.
			Command: strings.TrimRight(line, "\r\n"),
		}}})
		return nil
	}
}

func (m shellModel) ackShellCmd(offset uint64) tea.Cmd {
	return func() tea.Msg {
		_ = m.stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
			Name:      m.name,
			ClientId:  m.clientID,
			AckOffset: offset,
		}}})
		return nil
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.recvEventCmd(), tea.EnterAltScreen)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = t.Width
		m.composer.SetWidth(t.Width - 2)
		m.composer.SetHeight(minInt(8, maxInt(3, t.Height/5)))
		m.viewport.Height = t.Height - 1 - m.composer.Height()
		if m.viewport.Height < 1 {
			m.viewport.Height = 1
		}

		m.palette.SetWidth(minInt(80, t.Width-4))
		m.palette.SetHeight(minInt(12, maxInt(6, t.Height/3)))
		m.viewport.YPosition = 0
		return m, nil
	case tea.KeyMsg:
		switch t.String() {
		case "ctrl+c":
			m.cancel()
			_ = m.stream.CloseSend()
			return m, tea.Quit
		case "esc":
			if m.paletteOpen {
				m.paletteOpen = false
				return m, nil
			}
			m.cancel()
			_ = m.stream.CloseSend()
			return m, tea.Quit
		case "ctrl+p":
			m.paletteOpen = !m.paletteOpen
			return m, nil
		case "ctrl+s", "alt+enter":
			line := strings.TrimSpace(m.composer.Value())
			m.composer.SetValue("")
			if line == "" {
				return m, nil
			}
			if line == "/compact" {
				keep := 500
				if m.scrollback != nil && m.scrollback.maxLines > 0 {
					keep = minInt(keep, m.scrollback.maxLines)
				}
				m.scrollback.Compact(keep)
				m.renderScrollback()
				return m, nil
			}
			if line == "/help" {
				m.append("[help] Ctrl+S send | Ctrl+P palette | Esc close palette/quit | /compact | /shell | /write | /sessions\n")
				return m, nil
			}
			if !strings.HasPrefix(line, "/") {
				m.append(fmt.Sprintf("[local] you:\n%s\n", line))
			}
			return m, tea.Batch(m.sendLineCmd(line), m.recvEventCmd())
		}
	case eventMsg:
		if ev := t.ev; ev != nil && ev.GetLane() == xrunnerv1.EventLane_EVENT_LANE_LOW {
			if chunk := ev.GetToolOutputChunk(); chunk != nil {
				if strings.EqualFold(strings.TrimSpace(chunk.GetStream()), "assistant") {
					m.assistantActive = true
					in := string(chunk.GetData())
					out := in
					if m.assistantFull != "" && strings.HasPrefix(in, m.assistantFull) {
						out = in[len(m.assistantFull):]
						m.assistantFull = in
					} else if m.assistantFull != "" && strings.HasPrefix(m.assistantFull, in) {
						out = ""
					} else {
						m.assistantFull += in
					}
					if out != "" {
						m.assistantLive.WriteString(out)
					}
				} else {
					m.pendingTool.Write(chunk.GetData())
				}
				if !m.flushScheduled {
					m.flushScheduled = true
					return m, tea.Batch(m.recvEventCmd(), m.flushCmd())
				}
				return m, m.recvEventCmd()
			}
		}
		if m.assistantActive && m.assistantLive.Len() > 0 {
			m.append("[agent] " + m.assistantLive.String() + "\n")
			m.assistantLive.Reset()
			m.assistantActive = false
			m.assistantFull = ""
		}
		m.append(renderEvent(t.ev))
		return m, m.recvEventCmd()
	case textMsg:
		m.append(t.text)
		return m, nil
	case flushMsg:
		m.flushScheduled = false
		if m.pendingTool.Len() > 0 {
			m.append(m.pendingTool.String())
			m.pendingTool.Reset()
		}
		m.renderScrollback()
		return m, nil
	case errMsg:
		m.status = "error: " + t.err.Error()
		m.append(m.status + "\n")
		return m, nil
	}

	var cmd tea.Cmd
	if m.paletteOpen {
		var c tea.Cmd
		m.palette, c = m.palette.Update(msg)
		if km, ok := msg.(tea.KeyMsg); ok && km.String() == "enter" {
			if it, ok := m.palette.SelectedItem().(paletteItem); ok {
				m.paletteOpen = false
				if it.action != nil {
					return m, it.action(&m)
				}
				if it.insert != "" {
					m.composer.SetValue(it.insert)
				}
				return m, nil
			}
		}
		return m, c
	}
	m.composer, cmd = m.composer.Update(msg)
	return m, cmd
}

func (m model) View() string {
	keptLines, keptBytes, droppedLines := 0, 0, 0
	if m.scrollback != nil {
		keptLines, keptBytes, droppedLines, _ = m.scrollback.Stats()
	}
	approxTokens := keptBytes / 4
	header := fmt.Sprintf("xrunner session %s | %s | ctx %dL(+%d) ~%dtok | Ctrl+S send • Ctrl+P palette\n",
		m.sessionID, m.status, keptLines, droppedLines, approxTokens)
	out := header + m.viewport.View() + "\n" + m.composer.View()
	if m.paletteOpen {
		out += "\n\n" + m.palette.View()
	}
	return out
}

func (m *model) append(s string) {
	if m.scrollback == nil {
		m.scrollback = newScrollback(5000, 1<<20)
	}
	m.scrollback.Append(s)
	m.renderScrollback()
}

func (m *model) renderScrollback() {
	if m.scrollback == nil {
		return
	}
	wasAtBottom := m.viewport.AtBottom()
	content := m.scrollback.Content()
	if m.assistantActive && m.assistantLive.Len() > 0 {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "[agent] " + m.assistantLive.String()
	}
	m.viewport.SetContent(content)
	if wasAtBottom {
		m.viewport.GotoBottom()
	}
}

func (m model) recvEventCmd() tea.Cmd {
	return func() tea.Msg {
		ev, err := m.stream.Recv()
		if err != nil {
			return errMsg{err: err}
		}
		return eventMsg{ev: ev}
	}
}

func (m model) flushCmd() tea.Cmd {
	return tea.Tick(m.flushEvery, func(time.Time) tea.Msg { return flushMsg{} })
}

func (m model) sendLineCmd(line string) tea.Cmd {
	return func() tea.Msg {
		if strings.HasPrefix(line, "/shell ") {
			payload := map[string]any{"cmd": strings.TrimSpace(strings.TrimPrefix(line, "/shell ")), "pty": true}
			b, _ := json.Marshal(payload)
			_ = m.stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
				Name:     "shell",
				ArgsJson: string(b),
			}}})
			return nil
		}
		if strings.HasPrefix(line, "/write ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "/write "))
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) != 2 {
				_ = m.stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: "usage: /write <path> <content>"}}})
				return nil
			}
			payload := map[string]any{"path": parts[0], "content": parts[1], "mode": 420}
			b, _ := json.Marshal(payload)
			_ = m.stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_ToolInvoke{ToolInvoke: &xrunnerv1.ToolInvoke{
				Name:     "write_file_atomic",
				ArgsJson: string(b),
			}}})
			return nil
		}
		if line == "/sessions" {
			resp, err := m.svc.ListSessions(m.ctx, &xrunnerv1.ListSessionsRequest{})
			if err != nil {
				return errMsg{err: err}
			}
			var b strings.Builder
			b.WriteString("sessions:\n")
			for _, s := range resp.GetSessions() {
				b.WriteString(fmt.Sprintf("  %s  root=%s\n", s.GetSessionId(), s.GetWorkspaceRoot()))
			}
			return textMsg{text: b.String()}
		}

		_ = m.stream.Send(&xrunnerv1.ClientMsg{Kind: &xrunnerv1.ClientMsg_UserMessage{UserMessage: &xrunnerv1.UserMessage{Text: line}}})
		return nil
	}
}

func renderEvent(ev *xrunnerv1.AgentEvent) string {
	if ev == nil {
		return ""
	}
	ts := time.Now().Format("15:04:05")
	if ev.GetTimestamp() != nil {
		ts = ev.GetTimestamp().AsTime().Format("15:04:05")
	}
	switch k := ev.GetKind().(type) {
	case *xrunnerv1.AgentEvent_UserMessage:
		return fmt.Sprintf("[%s] you: %s\n", ts, k.UserMessage.GetText())
	case *xrunnerv1.AgentEvent_AssistantMessage:
		return fmt.Sprintf("[%s] agent: %s\n", ts, k.AssistantMessage.GetText())
	case *xrunnerv1.AgentEvent_ToolCallStarted:
		return fmt.Sprintf("[%s] tool start: %s (%s)\n", ts, k.ToolCallStarted.GetName(), k.ToolCallStarted.GetToolCallId())
	case *xrunnerv1.AgentEvent_ToolOutputChunk:
		return fmt.Sprintf("[%s] %s: %s", ts, k.ToolOutputChunk.GetStream(), string(k.ToolOutputChunk.GetData()))
	case *xrunnerv1.AgentEvent_ToolCallResult:
		if k.ToolCallResult.GetErrorMessage() != "" {
			return fmt.Sprintf("[%s] tool result: exit=%d err=%s\n", ts, k.ToolCallResult.GetExitCode(), k.ToolCallResult.GetErrorMessage())
		}
		return fmt.Sprintf("[%s] tool result: exit=%d\n", ts, k.ToolCallResult.GetExitCode())
	case *xrunnerv1.AgentEvent_FileDiff:
		return fmt.Sprintf("[%s] diff %s:\n%s\n", ts, k.FileDiff.GetPath(), k.FileDiff.GetUnifiedDiff())
	case *xrunnerv1.AgentEvent_Status:
		return fmt.Sprintf("[%s] status: %s %s\n", ts, k.Status.GetPhase(), k.Status.GetDetail())
	case *xrunnerv1.AgentEvent_Error:
		return fmt.Sprintf("[%s] error: %s\n", ts, k.Error.GetMessage())
	default:
		return fmt.Sprintf("[%s] event %d\n", ts, ev.GetEventId())
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
