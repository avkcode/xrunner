package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	cliconfig "github.com/antonkrylov/xrunner/internal/cli/config"
	"github.com/antonkrylov/xrunner/internal/client"
)

type logPane struct {
	mu        sync.Mutex
	text      string
	data      binding.String
	label     *widget.Label
	scroll    *container.Scroll
	maxBytes  int
	attached  bool
	lastError error
}

type goTestFailureArtifactPayload struct {
	Packages []string          `json:"packages"`
	Tests    []string          `json:"tests"`
	Files    []goTestFileFrame `json:"files"`
}

type goTestFileFrame struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func newLogPane(maxBytes int) *logPane {
	d := binding.NewString()
	l := widget.NewLabelWithData(d)
	l.Wrapping = fyne.TextWrapWord
	s := container.NewVScroll(l)
	return &logPane{data: d, label: l, scroll: s, maxBytes: maxBytes}
}

func (p *logPane) appendLine(s string) {
	p.mu.Lock()
	p.text += s
	if p.maxBytes > 0 && len(p.text) > p.maxBytes {
		p.text = p.text[len(p.text)-p.maxBytes:]
	}
	next := p.text
	p.mu.Unlock()
	_ = p.data.Set(next)
}

func main() {
	a := app.NewWithID("xrunner.desktop")
	w := a.NewWindow("xrunner")
	w.Resize(fyne.NewSize(1080, 720))

	configPathEntry := widget.NewEntry()
	if v := strings.TrimSpace(os.Getenv("XRUNNER_CONFIG")); v != "" {
		configPathEntry.SetText(v)
	} else {
		configPathEntry.SetText(cliconfig.DefaultConfigPath())
	}
	contextSelect := widget.NewSelect(nil, func(_ string) {})
	apiAddrEntry := widget.NewEntry()
	timeoutEntry := widget.NewEntry()
	timeoutEntry.SetPlaceHolder("0s (use config/default)")
	tenantEntry := widget.NewEntry()
	tenantEntry.SetText("default")
	jobIDEntry := widget.NewEntry()
	useTLSCheck := widget.NewCheck("TLS", func(bool) {})

	statusData := binding.NewString()
	_ = statusData.Set("Ready")
	status := widget.NewLabelWithData(statusData)

	stdout := newLogPane(512 * 1024)
	stderr := newLogPane(512 * 1024)
	shellOut := newLogPane(512 * 1024)
	shellReplay := newLogPane(512 * 1024)
	artifactBox := container.NewVBox()
	artifactScroll := container.NewVScroll(artifactBox)

	type streamState struct {
		cancel context.CancelFunc
		conn   io.Closer
	}
	var stMu sync.Mutex
	var st *streamState

	stopStreaming := func() {
		stMu.Lock()
		defer stMu.Unlock()
		if st == nil {
			return
		}
		st.cancel()
		_ = st.conn.Close()
		st = nil
	}

	loadConfig := func() {
		cfg, err := cliconfig.Load(strings.TrimSpace(configPathEntry.Text))
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		var contexts []string
		if cfg != nil {
			for k := range cfg.Contexts {
				contexts = append(contexts, k)
			}
			sort.Strings(contexts)
		}
		contextSelect.Options = contexts
		if cfg != nil && cfg.CurrentContext != "" {
			contextSelect.SetSelected(cfg.CurrentContext)
		} else if len(contexts) > 0 && contextSelect.Selected == "" {
			contextSelect.SetSelected(contexts[0])
		}
		contextSelect.Refresh()
	}

	parseTimeout := func() (time.Duration, error) {
		raw := strings.TrimSpace(timeoutEntry.Text)
		if raw == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid timeout %q: %w", raw, err)
		}
		return d, nil
	}

	startTail := func() {
		stopStreaming()

		timeout, err := parseTimeout()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		resolved, err := client.ResolveConnection(
			strings.TrimSpace(configPathEntry.Text),
			strings.TrimSpace(contextSelect.Selected),
			strings.TrimSpace(apiAddrEntry.Text),
			timeout,
		)
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		tenantID := strings.TrimSpace(tenantEntry.Text)
		jobID := strings.TrimSpace(jobIDEntry.Text)
		if tenantID == "" || jobID == "" {
			dialog.ShowError(fmt.Errorf("tenant and job ID are required"), w)
			return
		}

		mode := client.DialInsecure
		if useTLSCheck.Checked {
			mode = client.DialTLS
		}

		dialCtx, cancelDial := context.WithTimeout(context.Background(), resolved.Timeout)
		js, conn, err := client.DialJobService(dialCtx, resolved.APIAddr, mode)
		cancelDial()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		streamCtx, cancelStream := context.WithCancel(context.Background())
		stream, err := js.StreamJobLogs(streamCtx, &xrunnerv1.StreamJobLogsRequest{
			Ref: &xrunnerv1.JobRef{TenantId: tenantID, JobId: jobID},
		})
		if err != nil {
			cancelStream()
			_ = conn.Close()
			dialog.ShowError(err, w)
			return
		}

		stMu.Lock()
		st = &streamState{cancel: cancelStream, conn: conn}
		stMu.Unlock()

		_ = statusData.Set(fmt.Sprintf("Tailing %s/%s via %s", tenantID, jobID, resolved.APIAddr))

		go func() {
			defer stopStreaming()
			for {
				chunk, err := stream.Recv()
				if err == io.EOF || isCanceled(err) {
					return
				}
				if err != nil {
					_ = statusData.Set("Stream error: " + err.Error())
					return
				}
				ts := "<unknown>"
				if chunk.GetTimestamp() != nil {
					ts = chunk.GetTimestamp().AsTime().Format(time.RFC3339)
				}
				line := fmt.Sprintf("[%s] %s", ts, string(chunk.GetData()))
				switch strings.ToLower(strings.TrimSpace(chunk.GetStream())) {
				case "stderr":
					stderr.appendLine(line)
				case "stdout":
					stdout.appendLine(line)
				default:
					stdout.appendLine(fmt.Sprintf("[%s][%s] %s", ts, chunk.GetStream(), string(chunk.GetData())))
				}
			}
		}()
	}

	clearLogs := func() {
		stdout.mu.Lock()
		stdout.text = ""
		stdout.mu.Unlock()
		stderr.mu.Lock()
		stderr.text = ""
		stderr.mu.Unlock()
		_ = stdout.data.Set("")
		_ = stderr.data.Set("")
	}

	remoteAddrEntry := widget.NewEntry()
	remoteAddrEntry.SetText("127.0.0.1:7337")
	shellNameEntry := widget.NewEntry()
	shellNameEntry.SetPlaceHolder("ops")
	shellInput := widget.NewEntry()
	shellInput.SetPlaceHolder("command (sent to durable shell)")

	var timelineMu sync.Mutex
	var timeline []*xrunnerv1.ShellCommandRecord
	var selected *xrunnerv1.ShellCommandRecord
	var timelineList *widget.List
	var rerunOnClick bool
	var loadArtifacts func()
	var rerunSelected func()
	var openFileAt func(path string, line int)
	var fixAndRerun func(payload *goTestFailureArtifactPayload)
	var rerunFailingTest func(payload *goTestFailureArtifactPayload)

	type shellState struct {
		cancel context.CancelFunc
		conn   *grpc.ClientConn
		stream xrunnerv1.RemoteShellService_AttachShellClient
	}
	var shMu sync.Mutex
	var sh *shellState

	stopShell := func() {
		shMu.Lock()
		defer shMu.Unlock()
		if sh == nil {
			return
		}
		sh.cancel()
		_ = sh.stream.CloseSend()
		_ = sh.conn.Close()
		sh = nil
	}

	startShell := func() {
		stopShell()

		addr := strings.TrimSpace(remoteAddrEntry.Text)
		name := strings.TrimSpace(shellNameEntry.Text)
		if addr == "" || name == "" {
			dialog.ShowError(fmt.Errorf("remote addr and shell name are required"), w)
			return
		}

		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}

		shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := shellc.EnsureShellSession(ctx0, &xrunnerv1.EnsureShellSessionRequest{Name: name}); err != nil {
			cancel0()
			_ = conn.Close()
			dialog.ShowError(err, w)
			return
		}
		cancel0()

		ctx, cancel := context.WithCancel(context.Background())
		stream, err := shellc.AttachShell(ctx)
		if err != nil {
			cancel()
			_ = conn.Close()
			dialog.ShowError(err, w)
			return
		}
		if err := stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       0,
			ClientId:          "desktop",
			ResumeFromLastAck: true,
			MaxChunkBytes:     0,
			PollMs:            0,
		}}}); err != nil {
			cancel()
			_ = conn.Close()
			dialog.ShowError(err, w)
			return
		}

		shMu.Lock()
		sh = &shellState{cancel: cancel, conn: conn, stream: stream}
		shMu.Unlock()

		_ = statusData.Set("Shell attached: " + name + " via " + addr)

		go func() {
			defer stopShell()
			for {
				msg, err := stream.Recv()
				if err == io.EOF || isCanceled(err) {
					return
				}
				if err != nil {
					_ = statusData.Set("Shell error: " + err.Error())
					return
				}
				if ce := msg.GetCommand(); ce != nil && ce.GetRecord() != nil && ce.GetPhase() == xrunnerv1.ShellServerCommandEvent_PHASE_COMPLETED {
					timelineMu.Lock()
					timeline = append(timeline, ce.GetRecord())
					timelineMu.Unlock()
					if timelineList != nil {
						timelineList.Refresh()
					}
					if loadArtifacts != nil {
						go loadArtifacts()
					}
				}
				if chunk := msg.GetChunk(); chunk != nil && len(chunk.GetData()) > 0 {
					shellOut.appendLine(string(chunk.GetData()))
					u := chunk.GetUncompressedLen()
					if u == 0 {
						u = uint32(len(chunk.GetData()))
					}
					_ = stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
						Name:      name,
						ClientId:  "desktop",
						AckOffset: chunk.GetOffset() + uint64(u),
					}}})
				}
			}
		}()
	}

	openFileAt = func(path string, line int) {
		addr := strings.TrimSpace(remoteAddrEntry.Text)
		if addr == "" {
			return
		}
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}

		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			_ = statusData.Set("open file dial: " + err.Error())
			return
		}
		defer conn.Close()
		files := xrunnerv1.NewRemoteFileServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 8*time.Second)
		rf, err := files.ReadFile(ctx0, &xrunnerv1.ReadFileRequest{Path: path})
		cancel0()
		if err != nil {
			_ = statusData.Set("read file: " + err.Error())
			return
		}

		dir := filepath.Join(os.TempDir(), "xrunner-open")
		dst := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			_ = statusData.Set("mkdir: " + err.Error())
			return
		}
		if err := os.WriteFile(dst, rf.GetData(), 0o644); err != nil {
			_ = statusData.Set("write temp: " + err.Error())
			return
		}

		// Prefer VS Code if present to jump to line.
		if _, err := exec.LookPath("code"); err == nil && line > 0 {
			_ = exec.Command("code", "-g", fmt.Sprintf("%s:%d:1", dst, line)).Start()
			return
		}
		// Fallback: OS opener.
		switch runtime.GOOS {
		case "darwin":
			_ = exec.Command("open", dst).Start()
		case "windows":
			_ = exec.Command("cmd", "/c", "start", "", dst).Start()
		default:
			_ = exec.Command("xdg-open", dst).Start()
		}
	}

	computeHeuristicFix := func(filePath string, line int, message string) (string, string, []byte, bool) {
		// Extract expected/got from message.
		expected, got, ok := parseExpectedGot(message)
		if !ok || expected == "" || got == "" {
			return "", "", nil, false
		}
		addr := strings.TrimSpace(remoteAddrEntry.Text)
		if addr == "" {
			return "", "", nil, false
		}
		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			return "", "", nil, false
		}
		defer conn.Close()
		files := xrunnerv1.NewRemoteFileServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 8*time.Second)
		rf, err := files.ReadFile(ctx0, &xrunnerv1.ReadFileRequest{Path: filePath})
		cancel0()
		if err != nil {
			return "", "", nil, false
		}
		lines := strings.Split(string(rf.GetData()), "\n")
		idx := line - 1
		if idx < 0 || idx >= len(lines) {
			return "", "", nil, false
		}
		origLine := lines[idx]
		newLine, ok := replaceLiteral(origLine, expected, got)
		if !ok {
			// Try a small window.
			for d := 1; d <= 3 && !ok; d++ {
				if idx-d >= 0 {
					if nl, ok2 := replaceLiteral(lines[idx-d], expected, got); ok2 {
						origLine = lines[idx-d]
						newLine = nl
						idx = idx - d
						ok = true
						break
					}
				}
				if idx+d < len(lines) {
					if nl, ok2 := replaceLiteral(lines[idx+d], expected, got); ok2 {
						origLine = lines[idx+d]
						newLine = nl
						idx = idx + d
						ok = true
						break
					}
				}
			}
		}
		if !ok || newLine == origLine {
			return "", "", nil, false
		}
		lines[idx] = newLine
		next := strings.Join(lines, "\n")
		return origLine, newLine, []byte(next), true
	}

	rerunFailingTest = func(payload *goTestFailureArtifactPayload) {
		shMu.Lock()
		cur := sh
		shMu.Unlock()
		if cur == nil || payload == nil {
			return
		}
		pkg := ""
		if len(payload.Packages) > 0 {
			pkg = strings.TrimSpace(payload.Packages[0])
		}
		test := ""
		if len(payload.Tests) > 0 {
			test = strings.TrimSpace(payload.Tests[0])
		}
		cmdStr := "go test ./..."
		if pkg != "" {
			cmdStr = "go test " + pkg
		}
		if test != "" {
			cmdStr = cmdStr + " -run '^" + test + "$' -count=1"
		}
		_ = cur.stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
			Name:    strings.TrimSpace(shellNameEntry.Text),
			Command: cmdStr,
		}}})
	}

	fixAndRerun = func(payload *goTestFailureArtifactPayload) {
		if payload == nil || len(payload.Files) == 0 {
			return
		}
		f := payload.Files[0]
		orig, patched, newContent, ok := computeHeuristicFix(f.File, f.Line, f.Text)
		if !ok {
			dialog.ShowInformation("Fix", "Could not auto-fix (heuristic) this failure. Opening file instead.", w)
			openFileAt(f.File, f.Line)
			return
		}
		dialog.ShowConfirm("Fix & rerun", fmt.Sprintf("Apply this patch?\n- %s\n+ %s", orig, patched), func(yes bool) {
			if !yes {
				return
			}
			addr := strings.TrimSpace(remoteAddrEntry.Text)
			if addr == "" {
				return
			}
			dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
			conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
			cancelDial()
			if err != nil {
				_ = statusData.Set("fix dial: " + err.Error())
				return
			}
			defer conn.Close()
			files := xrunnerv1.NewRemoteFileServiceClient(conn)
			ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
			_, err = files.WriteFileAtomic(ctx1, &xrunnerv1.WriteFileRequest{Path: f.File, Data: newContent, Mode: 0o644})
			cancel1()
			if err != nil {
				_ = statusData.Set("write fix: " + err.Error())
				return
			}
			rerunFailingTest(payload)
		}, w)
	}

	sendShell := func() {
		shMu.Lock()
		cur := sh
		shMu.Unlock()
		if cur == nil {
			dialog.ShowError(fmt.Errorf("shell not attached"), w)
			return
		}
		line := strings.TrimSpace(shellInput.Text)
		if line == "" {
			return
		}
		shellInput.SetText("")
		_ = cur.stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
			Name:    strings.TrimSpace(shellNameEntry.Text),
			Command: line,
		}}})
	}

	loadConfig()

	settings := container.NewGridWithColumns(2,
		container.NewBorder(nil, nil, widget.NewLabel("Config path"), nil, configPathEntry),
		container.NewBorder(nil, nil, widget.NewButton("Reload contexts", loadConfig), nil, layout.NewSpacer()),
		container.NewBorder(nil, nil, widget.NewLabel("Context"), nil, contextSelect),
		container.NewBorder(nil, nil, widget.NewLabel("API addr override"), nil, apiAddrEntry),
		container.NewBorder(nil, nil, widget.NewLabel("Timeout"), nil, timeoutEntry),
		container.NewBorder(nil, nil, useTLSCheck, nil, layout.NewSpacer()),
	)

	controls := container.NewHBox(
		widget.NewLabel("Tenant"),
		tenantEntry,
		layout.NewSpacer(),
		widget.NewLabel("Job ID"),
		jobIDEntry,
		widget.NewButton("Tail logs", startTail),
		widget.NewButton("Stop", stopStreaming),
		widget.NewButton("Clear", clearLogs),
	)

	logTabs := container.NewAppTabs(
		container.NewTabItem("stdout", stdout.scroll),
		container.NewTabItem("stderr", stderr.scroll),
		container.NewTabItem("shell", shellOut.scroll),
		container.NewTabItem("shell-replay", shellReplay.scroll),
		container.NewTabItem("shell-artifacts", artifactScroll),
	)

	shellControls := container.NewHBox(
		widget.NewLabel("Remote"),
		remoteAddrEntry,
		layout.NewSpacer(),
		widget.NewLabel("Shell"),
		shellNameEntry,
		widget.NewButton("Attach", startShell),
		widget.NewButton("Stop", stopShell),
	)

	shellSend := container.NewHBox(
		shellInput,
		widget.NewButton("Send", sendShell),
	)

	refreshTimeline := func() {
		addr := strings.TrimSpace(remoteAddrEntry.Text)
		name := strings.TrimSpace(shellNameEntry.Text)
		if addr == "" || name == "" {
			dialog.ShowError(fmt.Errorf("remote addr and shell name are required"), w)
			return
		}
		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		defer conn.Close()
		shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := shellc.ListShellCommands(ctx0, &xrunnerv1.ListShellCommandsRequest{Name: name, Limit: 100})
		cancel0()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		timelineMu.Lock()
		timeline = resp.GetCommands()
		selected = nil
		timelineMu.Unlock()
		if timelineList != nil {
			timelineList.Refresh()
		}
	}

	loadArtifacts = func() {
		addr := strings.TrimSpace(remoteAddrEntry.Text)
		name := strings.TrimSpace(shellNameEntry.Text)
		timelineMu.Lock()
		rec := selected
		timelineMu.Unlock()
		if addr == "" || name == "" || rec == nil {
			return
		}
		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			return
		}
		defer conn.Close()
		shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 8*time.Second)
		resp, err := shellc.ListShellArtifacts(ctx0, &xrunnerv1.ListShellArtifactsRequest{Name: name, CommandId: rec.GetCommandId(), Limit: 50})
		cancel0()
		if err != nil {
			return
		}
		artifactBox.Objects = nil
		for _, a := range resp.GetArtifacts() {
			if a == nil {
				continue
			}
			var payload goTestFailureArtifactPayload
			_ = json.Unmarshal(a.GetDataJson(), &payload)

			content := container.NewVBox()
			if len(payload.Tests) > 0 {
				content.Add(widget.NewLabel("Tests: " + strings.Join(payload.Tests, ", ")))
			}
			if len(payload.Packages) > 0 {
				content.Add(widget.NewLabel("Packages: " + strings.Join(payload.Packages, ", ")))
			}
			if len(payload.Files) > 0 {
				content.Add(widget.NewLabel("Files:"))
				for _, fr := range payload.Files {
					fr := fr
					btn := widget.NewButton(fmt.Sprintf("%s:%d", fr.File, fr.Line), func() {
						if openFileAt != nil {
							openFileAt(fr.File, fr.Line)
						}
					})
					msg := widget.NewLabel(fr.Text)
					msg.Wrapping = fyne.TextWrapWord
					content.Add(container.NewVBox(btn, msg))
				}
			}

			actions := container.NewHBox(
				widget.NewButton("Open first file", func() {
					if len(payload.Files) > 0 && openFileAt != nil {
						openFileAt(payload.Files[0].File, payload.Files[0].Line)
					}
				}),
				widget.NewButton("Rerun failing test", func() {
					if rerunFailingTest != nil {
						rerunFailingTest(&payload)
					}
				}),
				widget.NewButton("Fix & rerun", func() {
					if fixAndRerun != nil {
						fixAndRerun(&payload)
					}
				}),
				widget.NewButton("Rerun full command", func() {
					if rerunSelected != nil {
						rerunSelected()
					}
				}),
			)

			card := widget.NewCard(a.GetTitle(), a.GetType(), container.NewVBox(content, actions))
			artifactBox.Add(card)
		}
		artifactBox.Refresh()
	}

	replaySelected := func() {
		addr := strings.TrimSpace(remoteAddrEntry.Text)
		name := strings.TrimSpace(shellNameEntry.Text)
		timelineMu.Lock()
		rec := selected
		timelineMu.Unlock()
		if addr == "" || name == "" || rec == nil {
			dialog.ShowError(fmt.Errorf("select a command in timeline"), w)
			return
		}
		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 25*time.Second)
		stream, err := shellc.AttachShell(ctx0)
		if err != nil {
			cancel0()
			_ = conn.Close()
			dialog.ShowError(err, w)
			return
		}
		if err := stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Hello{Hello: &xrunnerv1.ShellClientHello{
			Name:              name,
			AfterOffset:       rec.GetBeginOffset(),
			ClientId:          "desktop-replay",
			ResumeFromLastAck: false,
			MaxChunkBytes:     0,
			PollMs:            0,
			Compression:       xrunnerv1.ShellClientHello_COMPRESSION_NONE,
		}}}); err != nil {
			cancel0()
			_ = conn.Close()
			dialog.ShowError(err, w)
			return
		}
		shellReplay.mu.Lock()
		shellReplay.text = ""
		shellReplay.mu.Unlock()
		_ = shellReplay.data.Set("")

		go func() {
			defer cancel0()
			defer conn.Close()
			end := rec.GetEndOffset()
			// Wait for server hello before consuming chunks.
			for {
				msg, err := stream.Recv()
				if err == io.EOF || isCanceled(err) {
					return
				}
				if err != nil {
					_ = statusData.Set("Replay error: " + err.Error())
					return
				}
				if msg.GetHello() != nil {
					break
				}
			}
			for {
				msg, err := stream.Recv()
				if err == io.EOF || isCanceled(err) {
					return
				}
				if err != nil {
					_ = statusData.Set("Replay error: " + err.Error())
					return
				}
				ch := msg.GetChunk()
				if ch == nil || len(ch.GetData()) == 0 {
					continue
				}
				start := ch.GetOffset()
				adv := uint64(ch.GetUncompressedLen())
				if adv == 0 {
					adv = uint64(len(ch.GetData()))
				}
				data := ch.GetData()
				if start >= end {
					return
				}
				// Data is already marker-stripped; use uncompressed_len for boundary.
				if start+adv > end {
					// Best-effort: if we overshoot, just stop after printing this chunk.
					shellReplay.appendLine(string(data))
					return
				}
				shellReplay.appendLine(string(data))
				_ = stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Ack{Ack: &xrunnerv1.ShellClientAck{
					Name:      name,
					ClientId:  "desktop-replay",
					AckOffset: start + adv,
				}}})
				if start+adv >= end {
					return
				}
			}
		}()
	}

	rerunSelected = func() {
		shMu.Lock()
		cur := sh
		shMu.Unlock()
		timelineMu.Lock()
		rec := selected
		timelineMu.Unlock()
		if cur == nil || rec == nil {
			dialog.ShowError(fmt.Errorf("attach shell and select a command"), w)
			return
		}
		cmdStr := strings.TrimSpace(rec.GetCommand())
		if cmdStr == "" {
			return
		}
		_ = cur.stream.Send(&xrunnerv1.ShellClientMsg{Kind: &xrunnerv1.ShellClientMsg_Run{Run: &xrunnerv1.ShellClientRun{
			Name:    strings.TrimSpace(shellNameEntry.Text),
			Command: cmdStr,
		}}})
	}

	timelineList = widget.NewList(
		func() int {
			timelineMu.Lock()
			defer timelineMu.Unlock()
			return len(timeline)
		},
		func() fyne.CanvasObject { return widget.NewLabel("loading") },
		func(i widget.ListItemID, obj fyne.CanvasObject) {
			lbl := obj.(*widget.Label)
			timelineMu.Lock()
			defer timelineMu.Unlock()
			if i < 0 || i >= len(timeline) || timeline[i] == nil {
				lbl.SetText("")
				return
			}
			r := timeline[i]
			ts := ""
			if r.GetStartedAt() != nil {
				ts = r.GetStartedAt().AsTime().Format("15:04:05")
			}
			arts := ""
			if n := len(r.GetArtifactIds()); n > 0 {
				arts = fmt.Sprintf(" artifacts=%d", n)
			}
			lbl.SetText(fmt.Sprintf("%s exit=%d bytes=%d%s %s", ts, r.GetExitCode(), r.GetOutputBytes(), arts, r.GetCommand()))
		},
	)
	timelineList.OnSelected = func(id widget.ListItemID) {
		timelineMu.Lock()
		if id >= 0 && id < len(timeline) {
			selected = timeline[id]
		}
		timelineMu.Unlock()
		if loadArtifacts != nil {
			go loadArtifacts()
		}
		if rerunOnClick {
			go rerunSelected()
		}
	}

	searchEntry := widget.NewEntry()
	searchEntry.SetPlaceHolder("search in shell log")
	searchBtn := widget.NewButton("Search", func() {
		addr := strings.TrimSpace(remoteAddrEntry.Text)
		name := strings.TrimSpace(shellNameEntry.Text)
		q := strings.TrimSpace(searchEntry.Text)
		if addr == "" || name == "" || q == "" {
			dialog.ShowError(fmt.Errorf("remote addr, shell name, and query are required"), w)
			return
		}
		dialCtx, cancelDial := context.WithTimeout(context.Background(), 8*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelDial()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		defer conn.Close()
		shellc := xrunnerv1.NewRemoteShellServiceClient(conn)
		ctx0, cancel0 := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := shellc.SearchShell(ctx0, &xrunnerv1.ShellSearchRequest{
			Name:         name,
			Query:        []byte(q),
			MaxMatches:   50,
			ContextBytes: 120,
		})
		cancel0()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		shellReplay.mu.Lock()
		shellReplay.text = ""
		shellReplay.mu.Unlock()
		_ = shellReplay.data.Set("")
		for _, m := range resp.GetMatches() {
			if m == nil {
				continue
			}
			prefix := fmt.Sprintf("off=%d ", m.GetOffset())
			if strings.TrimSpace(m.GetCommandId()) != "" {
				prefix = fmt.Sprintf("off=%d cmd=%s ", m.GetOffset(), m.GetCommandId())
			}
			shellReplay.appendLine(prefix + string(m.GetSnippet()) + "\n")
		}
	})

	timelineControls := container.NewHBox(
		widget.NewButton("Refresh timeline", refreshTimeline),
		widget.NewButton("Replay selected", replaySelected),
		widget.NewButton("Rerun selected", rerunSelected),
		widget.NewCheck("Rerun on click", func(v bool) { rerunOnClick = v }),
		layout.NewSpacer(),
		searchEntry,
		searchBtn,
	)

	timelinePanel := container.NewBorder(timelineControls, nil, nil, nil, timelineList)
	logTabs.Append(container.NewTabItem("shell-cmds", timelinePanel))

	w.SetContent(container.NewBorder(
		container.NewVBox(settings, controls, widget.NewSeparator(), shellControls, shellSend),
		status,
		nil,
		nil,
		logTabs,
	))

	w.SetCloseIntercept(func() {
		stopStreaming()
		stopShell()
		w.Close()
	})
	w.ShowAndRun()
}

func isCanceled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.Canceled {
		return true
	}
	return false
}

func parseExpectedGot(s string) (expected string, got string, ok bool) {
	// A tiny heuristic for common patterns:
	// "expected 1, got 2"
	// "expected \"foo\", got \"bar\""
	s = strings.TrimSpace(s)
	l := strings.ToLower(s)
	i := strings.Index(l, "expected ")
	if i < 0 {
		return "", "", false
	}
	rest := s[i+len("expected "):]
	j := strings.Index(strings.ToLower(rest), ", got ")
	if j < 0 {
		return "", "", false
	}
	expected = strings.TrimSpace(rest[:j])
	got = strings.TrimSpace(rest[j+len(", got "):])
	// Trim trailing punctuation.
	got = strings.TrimRight(got, ".")
	return expected, got, expected != "" && got != ""
}

func replaceLiteral(line string, expected string, got string) (string, bool) {
	// Prefer exact token replacements for numbers and quoted strings.
	line2 := line
	if expected == got {
		return line, false
	}
	// If expected is quoted, replace the quoted string.
	if strings.HasPrefix(expected, "\"") && strings.HasSuffix(expected, "\"") {
		if strings.Contains(line2, expected) {
			return strings.Replace(line2, expected, got, 1), true
		}
	}
	// Numeric best-effort replacement.
	if isDigits(expected) && isDigits(got) {
		// Avoid replacing larger numbers containing the expected as substring.
		needle := expected
		for _, sep := range []string{" ", "\t", ")", "]", "}", ",", ";"} {
			if strings.Contains(line2, needle+sep) {
				return strings.Replace(line2, needle+sep, got+sep, 1), true
			}
		}
		if strings.Contains(line2, needle) {
			return strings.Replace(line2, needle, got, 1), true
		}
	}
	// Generic fallback.
	if strings.Contains(line2, expected) {
		return strings.Replace(line2, expected, got, 1), true
	}
	return line, false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
