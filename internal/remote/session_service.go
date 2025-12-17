package remote

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
	"github.com/antonkrylov/xrunner/internal/llm"
	"github.com/antonkrylov/xrunner/internal/remote/sessionstore"
	"github.com/antonkrylov/xrunner/internal/toolspec"
)

type sessionService struct {
	xrunnerv1.UnimplementedSessionServiceServer

	store *sessionstore.Store
	log   *slog.Logger

	mu          sync.Mutex
	subscribers map[string]map[int64]*sessionSubscriber
	nextSubID   int64

	toolMu      sync.Mutex
	toolCancels map[string]context.CancelFunc

	bundleMu sync.Mutex
	bundles  map[string]*toolspec.Bundle

	workspaceRootDefault string

	llmCfg     llm.Config
	llmClient  llm.Client
	llmInitErr error
}

type sessionSubscriber struct {
	ch chan *xrunnerv1.AgentEvent
}

func newSessionService(store *sessionstore.Store, workspaceRootDefault string, logger *slog.Logger) *sessionService {
	cfg := llm.FromEnv()
	client, err := llm.NewClient(cfg)
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Enabled {
		apiKeyHash := ""
		if strings.TrimSpace(cfg.APIKey) != "" {
			sum := sha256.Sum256([]byte(cfg.APIKey))
			apiKeyHash = hex.EncodeToString(sum[:])[:12]
		}
		apiKeySource := "none"
		if strings.TrimSpace(os.Getenv("XR_LLM_API_KEY")) != "" {
			apiKeySource = "XR_LLM_API_KEY"
		} else if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
			apiKeySource = "OPENAI_API_KEY"
		}
		baseURLSource := "none"
		if strings.TrimSpace(os.Getenv("XR_LLM_BASE_URL")) != "" {
			baseURLSource = "XR_LLM_BASE_URL"
		} else if strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")) != "" {
			baseURLSource = "OPENAI_BASE_URL"
		}
		provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
		if provider == "" {
			provider = "openai_compat"
		}
		if (provider == "openai_compat" || provider == "openai" || provider == "deepseek") &&
			(strings.TrimSpace(cfg.APIKeyHeader) != "" || strings.TrimSpace(cfg.APIKeyValuePrefix) != "" ||
				strings.TrimSpace(cfg.AuthHeader) != "" || strings.TrimSpace(cfg.AuthValuePrefix) != "") {
			logger.Warn("llm auth overrides are ignored for openai-compatible providers; using Authorization: Bearer <key>",
				"provider", provider,
				"auth_header", cfg.AuthHeader,
				"auth_prefix", cfg.AuthValuePrefix,
				"api_key_header", cfg.APIKeyHeader,
				"api_key_prefix", cfg.APIKeyValuePrefix,
			)
		}
		url, uerr := cfg.ChatURL()
		if uerr != nil {
			logger.Warn("llm config: chat url parse failed", "err", uerr)
		} else {
			logger.Info("llm config",
				"enabled", cfg.Enabled,
				"provider", cfg.Provider,
				"model", cfg.Model,
				"base_url", cfg.BaseURL,
				"base_url_source", baseURLSource,
				"chat_path", cfg.ChatPath,
				"chat_url", url,
				"stream", cfg.Stream,
				"timeout", cfg.Timeout.String(),
				"max_tool_iters", cfg.MaxToolIters,
				"max_history_events", cfg.MaxHistoryEvents,
				"auth_header", "Authorization",
				"auth_prefix", "Bearer ",
				"api_key_header", "",
				"api_key_prefix", "",
				"api_key_source", apiKeySource,
				"api_key_len", len(cfg.APIKey),
				"api_key_sha256_12", apiKeyHash,
			)
		}
		if err != nil {
			logger.Error("llm init failed", "err", err)
		}
	}
	return &sessionService{
		store:                store,
		log:                  logger,
		subscribers:          make(map[string]map[int64]*sessionSubscriber),
		toolCancels:          make(map[string]context.CancelFunc),
		bundles:              make(map[string]*toolspec.Bundle),
		workspaceRootDefault: workspaceRootDefault,
		llmCfg:               cfg,
		llmClient:            client,
		llmInitErr:           err,
	}
}

func (s *sessionService) CreateSession(ctx context.Context, req *xrunnerv1.CreateSessionRequest) (*xrunnerv1.CreateSessionResponse, error) {
	root := strings.TrimSpace(req.GetWorkspaceRoot())
	if root == "" {
		root = s.workspaceRootDefault
	}
	toolBundle := strings.TrimSpace(req.GetToolBundlePath())
	sess, err := s.store.Create(root, toolBundle)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	started := &xrunnerv1.AgentEvent{
		SessionId: sess.GetSessionId(),
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
		Kind: &xrunnerv1.AgentEvent_SessionStarted{
			SessionStarted: &xrunnerv1.SessionStarted{
				WorkspaceRoot:  sess.GetWorkspaceRoot(),
				ToolBundlePath: sess.GetToolBundlePath(),
			},
		},
	}
	_, _ = s.appendAndBroadcast(started)
	if toolBundle != "" {
		b, err := toolspec.LoadBundle(toolBundle)
		if err != nil {
			_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
				SessionId: sess.GetSessionId(),
				Timestamp: timestamppb.Now(),
				Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
				Kind: &xrunnerv1.AgentEvent_Error{
					Error: &xrunnerv1.Error{Message: "tool bundle: " + err.Error()},
				},
			})
		} else {
			s.bundleMu.Lock()
			s.bundles[sess.GetSessionId()] = b
			s.bundleMu.Unlock()
			names := make([]string, 0, len(b.Tools))
			for _, t := range b.Tools {
				names = append(names, t.Function.Name)
			}
			_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
				SessionId: sess.GetSessionId(),
				Timestamp: timestamppb.Now(),
				Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
				Kind: &xrunnerv1.AgentEvent_ToolBundleLoaded{
					ToolBundleLoaded: &xrunnerv1.ToolBundleLoaded{
						Path:          toolBundle,
						SchemaVersion: b.SchemaVersion,
						BundleVersion: b.BundleVersion,
						ToolNames:     names,
					},
				},
			})
		}
	}
	return &xrunnerv1.CreateSessionResponse{Session: sess}, nil
}

func (s *sessionService) ListSessions(ctx context.Context, req *xrunnerv1.ListSessionsRequest) (*xrunnerv1.ListSessionsResponse, error) {
	items, err := s.store.List()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &xrunnerv1.ListSessionsResponse{Sessions: items}, nil
}

func (s *sessionService) GetSession(ctx context.Context, req *xrunnerv1.GetSessionRequest) (*xrunnerv1.Session, error) {
	item, err := s.store.Get(strings.TrimSpace(req.GetSessionId()))
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return item, nil
}

func (s *sessionService) DeleteSession(ctx context.Context, req *xrunnerv1.GetSessionRequest) (*emptypb.Empty, error) {
	if err := s.store.Delete(strings.TrimSpace(req.GetSessionId())); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *sessionService) StreamEvents(req *xrunnerv1.StreamEventsRequest, stream xrunnerv1.SessionService_StreamEventsServer) error {
	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return status.Error(codes.InvalidArgument, "session_id is required")
	}
	after := req.GetAfterEventId()
	if err := s.store.Replay(sessionID, after, stream.Send); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	subID, sub := s.subscribe(sessionID)
	defer s.unsubscribe(sessionID, subID)
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case ev, ok := <-sub.ch:
			if err := sendEvent(stream, ev, ok, after); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
}

func (s *sessionService) Interact(stream xrunnerv1.SessionService_InteractServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be hello")
	}
	sessionID := strings.TrimSpace(hello.GetSessionId())
	after := hello.GetAfterEventId()

	if sessionID == "" {
		created, err := s.CreateSession(stream.Context(), &xrunnerv1.CreateSessionRequest{})
		if err != nil {
			return err
		}
		sessionID = created.GetSession().GetSessionId()
	}

	if err := s.store.Replay(sessionID, after, stream.Send); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	subID, sub := s.subscribe(sessionID)
	defer s.unsubscribe(sessionID, subID)

	msgCh := make(chan *xrunnerv1.ClientMsg, 32)
	recvErrCh := make(chan error, 1)
	go func() {
		defer close(msgCh)
		for {
			msg, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				recvErrCh <- err
				return
			}
			msgCh <- msg
		}
	}()

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case err := <-recvErrCh:
			return err
		case ev, ok := <-sub.ch:
			if err := sendEvent(stream, ev, ok, after); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		case msg, ok := <-msgCh:
			if !ok {
				// Client has closed their send side. Keep streaming events until the
				// client disconnects, but stop accepting new messages.
				msgCh = nil
				continue
			}
			switch {
			case msg.GetUserMessage() != nil:
				ev := &xrunnerv1.AgentEvent{
					SessionId: sessionID,
					Timestamp: timestamppb.Now(),
					Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
					Kind: &xrunnerv1.AgentEvent_UserMessage{
						UserMessage: &xrunnerv1.UserMessage{Text: msg.GetUserMessage().GetText()},
					},
				}
				_, _ = s.appendAndBroadcast(ev)
				if !s.llmCfg.Enabled {
					_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
						SessionId: sessionID,
						Timestamp: timestamppb.Now(),
						Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
						Kind: &xrunnerv1.AgentEvent_AssistantMessage{
							AssistantMessage: &xrunnerv1.AssistantMessage{
								Text:     "ack",
								Markdown: false,
							},
						},
					})
					continue
				}

				if s.llmInitErr != nil {
					_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
						SessionId: sessionID,
						Timestamp: timestamppb.Now(),
						Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
						Kind: &xrunnerv1.AgentEvent_Error{
							Error: &xrunnerv1.Error{Message: "llm init: " + s.llmInitErr.Error()},
						},
					})
					continue
				}
				if s.llmClient == nil {
					_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
						SessionId: sessionID,
						Timestamp: timestamppb.Now(),
						Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
						Kind: &xrunnerv1.AgentEvent_Error{
							Error: &xrunnerv1.Error{Message: "llm: enabled but client is nil (check XR_LLM_PROVIDER)"},
						},
					})
					continue
				}

				_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
					SessionId: sessionID,
					Timestamp: timestamppb.Now(),
					Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
					Kind: &xrunnerv1.AgentEvent_Status{
						Status: &xrunnerv1.Status{Phase: "thinking", Detail: "llm:" + s.llmCfg.Provider + " model=" + s.llmCfg.Model},
					},
				})

				msgs, merr := s.buildLLMMessages(sessionID)
				if merr != nil {
					_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
						SessionId: sessionID,
						Timestamp: timestamppb.Now(),
						Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
						Kind: &xrunnerv1.AgentEvent_Error{
							Error: &xrunnerv1.Error{Message: "llm context: " + merr.Error()},
						},
					})
					continue
				}

				var tools any
				toolCount := 0
				if bundle, _, _ := s.bundleForSession(sessionID); bundle != nil {
					tools = bundle.Tools
					toolCount = len(bundle.Tools)
				}

				assistantStreamID := ""
				var onDelta func(string)
				if s.llmCfg.Stream {
					assistantStreamID = "assistant:" + uuid.NewString()
					onDelta = func(delta string) {
						if delta == "" {
							return
						}
						s.broadcastEphemeral(&xrunnerv1.AgentEvent{
							SessionId: sessionID,
							Timestamp: timestamppb.Now(),
							Lane:      xrunnerv1.EventLane_EVENT_LANE_LOW,
							Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{
								ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
									ToolCallId: assistantStreamID,
									Stream:     "assistant",
									Data:       []byte(delta),
								},
							},
						})
					}
				}

				finalText := ""
				maxIters := s.llmCfg.MaxToolIters
				if maxIters <= 0 {
					maxIters = 1
				}
				chatURL := ""
				if u, err := s.llmCfg.ChatURL(); err == nil {
					chatURL = u
				}
				for iter := 0; iter < maxIters; iter++ {
					s.log.Debug("llm generate",
						"session_id", sessionID,
						"iter", iter,
						"provider", s.llmCfg.Provider,
						"model", s.llmCfg.Model,
						"stream", s.llmCfg.Stream,
						"tools", toolCount,
						"messages", len(msgs),
						"url", chatURL,
					)
					callCtx, cancel := context.WithTimeout(stream.Context(), s.llmCfg.Timeout)
					res, rerr := s.llmClient.Generate(callCtx, llm.Request{
						Messages:    msgs,
						Tools:       tools,
						OnTextDelta: onDelta,
					})
					cancel()
					if rerr != nil {
						s.log.Warn("llm generate failed",
							"session_id", sessionID,
							"iter", iter,
							"provider", s.llmCfg.Provider,
							"model", s.llmCfg.Model,
							"url", chatURL,
							"err", rerr,
						)
						_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
							SessionId: sessionID,
							Timestamp: timestamppb.Now(),
							Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
							Kind: &xrunnerv1.AgentEvent_Error{
								Error: &xrunnerv1.Error{Message: "llm: " + rerr.Error()},
							},
						})
						break
					}

					if len(res.ToolCalls) == 0 {
						finalText = res.Text
						break
					}

					// Normalize tool call ids for providers that omit them.
					for i := range res.ToolCalls {
						if strings.TrimSpace(res.ToolCalls[i].ID) == "" {
							res.ToolCalls[i].ID = fmt.Sprintf("call_%d", i)
						}
					}

					// Record assistant tool calls in the in-memory prompt.
					msgs = append(msgs, llm.Message{
						Role:      "assistant",
						Content:   res.Text,
						ToolCalls: res.ToolCalls,
					})

					// Execute tools synchronously and feed results back.
					for _, tc := range res.ToolCalls {
						inv := &xrunnerv1.ToolInvoke{Name: tc.Name, ArgsJson: tc.ArgsJSON}
						_, toolContent, _, _ := s.execToolSync(stream.Context(), sessionID, inv)
						msgs = append(msgs, llm.Message{
							Role:       "tool",
							ToolCallID: tc.ID,
							Content:    toolContent,
						})
					}
				}

				if strings.TrimSpace(finalText) != "" {
					evt := &xrunnerv1.AgentEvent{
						SessionId: sessionID,
						Timestamp: timestamppb.Now(),
						Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
						Kind: &xrunnerv1.AgentEvent_AssistantMessage{
							AssistantMessage: &xrunnerv1.AssistantMessage{
								Text:     strings.TrimSpace(finalText),
								Markdown: true,
							},
						},
					}
					// Avoid duplicating output when streaming deltas: persist the final message
					// but don't broadcast it (clients already saw the stream).
					if assistantStreamID != "" {
						_, _ = s.appendOnly(evt)
					} else {
						_, _ = s.appendAndBroadcast(evt)
					}
				}
				_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
					SessionId: sessionID,
					Timestamp: timestamppb.Now(),
					Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
					Kind: &xrunnerv1.AgentEvent_Status{
						Status: &xrunnerv1.Status{Phase: "idle", Detail: ""},
					},
				})
			case msg.GetToolInvoke() != nil:
				s.handleToolInvoke(sessionID, msg.GetToolInvoke())
			case msg.GetCancel() != nil:
				s.handleCancel(sessionID, msg.GetCancel())
			}
		}
	}
}

func (s *sessionService) buildLLMMessages(sessionID string) ([]llm.Message, error) {
	maxEvents := s.llmCfg.MaxHistoryEvents
	if maxEvents <= 0 {
		maxEvents = 50
	}

	// Replay events and keep only the last N, avoiding any new store API surface.
	ring := make([]*xrunnerv1.AgentEvent, 0, maxEvents)
	if err := s.store.Replay(sessionID, 0, func(ev *xrunnerv1.AgentEvent) error {
		if ev == nil {
			return nil
		}
		if len(ring) == maxEvents {
			copy(ring, ring[1:])
			ring[len(ring)-1] = ev
			return nil
		}
		ring = append(ring, ev)
		return nil
	}); err != nil {
		return nil, err
	}

	msgs := make([]llm.Message, 0, len(ring)+1)
	if strings.TrimSpace(s.llmCfg.SystemPrompt) != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: s.llmCfg.SystemPrompt})
	}
	for _, ev := range ring {
		switch k := ev.GetKind().(type) {
		case *xrunnerv1.AgentEvent_UserMessage:
			msgs = append(msgs, llm.Message{Role: "user", Content: k.UserMessage.GetText()})
		case *xrunnerv1.AgentEvent_AssistantMessage:
			msgs = append(msgs, llm.Message{Role: "assistant", Content: k.AssistantMessage.GetText()})
		}
	}
	return msgs, nil
}

func (s *sessionService) appendOnly(evt *xrunnerv1.AgentEvent) (*xrunnerv1.AgentEvent, error) {
	return s.store.Append(evt)
}

func (s *sessionService) broadcastEphemeral(evt *xrunnerv1.AgentEvent) {
	if evt == nil {
		return
	}
	if evt.Timestamp == nil {
		evt.Timestamp = timestamppb.Now()
	}
	s.broadcast(evt)
}

type limitedBuffer struct {
	max       int
	n         int
	truncated bool
	b         strings.Builder
}

func newLimitedBuffer(max int) *limitedBuffer {
	if max <= 0 {
		max = 64 * 1024
	}
	return &limitedBuffer{max: max}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.n >= b.max {
		b.truncated = true
		return len(p), nil
	}
	remain := b.max - b.n
	if len(p) > remain {
		_, _ = b.b.Write(p[:remain])
		b.n += remain
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.b.Write(p)
	b.n += len(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	if b == nil {
		return ""
	}
	s := b.b.String()
	if b.truncated {
		s += "\n[truncated]\n"
	}
	return s
}

func (s *sessionService) handleCancel(sessionID string, cancel *xrunnerv1.Cancel) {
	toolCallID := strings.TrimSpace(cancel.GetToolCallId())
	if toolCallID == "" {
		return
	}
	s.toolMu.Lock()
	fn := s.toolCancels[toolCallID]
	s.toolMu.Unlock()
	if fn != nil {
		fn()
	}
	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
		Kind: &xrunnerv1.AgentEvent_Status{
			Status: &xrunnerv1.Status{Phase: "idle", Detail: "canceled " + toolCallID},
		},
	})
}

func (s *sessionService) handleToolInvoke(sessionID string, inv *xrunnerv1.ToolInvoke) {
	tool := strings.TrimSpace(inv.GetName())
	if tool == "" {
		_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
			SessionId: sessionID,
			Timestamp: timestamppb.Now(),
			Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
			Kind: &xrunnerv1.AgentEvent_Error{
				Error: &xrunnerv1.Error{Message: "tool name is required"},
			},
		})
		return
	}

	bundle, bundlePath, bundleErr := s.bundleForSession(sessionID)
	if bundleErr != nil {
		_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
			SessionId: sessionID,
			Timestamp: timestamppb.Now(),
			Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
			Kind: &xrunnerv1.AgentEvent_Error{
				Error: &xrunnerv1.Error{Message: fmt.Sprintf("tool bundle %q: %v", bundlePath, bundleErr)},
			},
		})
		return
	}
	if bundle != nil && !bundleAllows(bundle, tool) {
		_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
			SessionId: sessionID,
			Timestamp: timestamppb.Now(),
			Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
			Kind: &xrunnerv1.AgentEvent_Error{
				Error: &xrunnerv1.Error{Message: "tool not permitted by bundle: " + tool},
			},
		})
		return
	}

	toolCallID := uuid.NewString()
	started := &xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
		Kind: &xrunnerv1.AgentEvent_ToolCallStarted{
			ToolCallStarted: &xrunnerv1.ToolCallStarted{
				ToolCallId: toolCallID,
				Name:       tool,
				ArgsJson:   inv.GetArgsJson(),
			},
		},
	}
	_, _ = s.appendAndBroadcast(started)
	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
		Kind: &xrunnerv1.AgentEvent_Status{
			Status: &xrunnerv1.Status{Phase: "running_tool", Detail: tool},
		},
	})

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.toolMu.Lock()
		s.toolCancels[toolCallID] = cancel
		s.toolMu.Unlock()
		defer func() {
			s.toolMu.Lock()
			delete(s.toolCancels, toolCallID)
			s.toolMu.Unlock()
		}()

		var exit int32
		var errMsg string
		switch tool {
		case "shell":
			exit, errMsg = s.execShellTool(ctx, sessionID, toolCallID, inv.GetArgsJson())
		case "write_file_atomic":
			exit, errMsg = s.execWriteFileTool(ctx, sessionID, toolCallID, inv.GetArgsJson())
		default:
			exit = 2
			errMsg = "unknown tool: " + tool
		}

		_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
			SessionId: sessionID,
			Timestamp: timestamppb.Now(),
			Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
			Kind: &xrunnerv1.AgentEvent_ToolCallResult{
				ToolCallResult: &xrunnerv1.ToolCallResult{
					ToolCallId:   toolCallID,
					ExitCode:     exit,
					ErrorMessage: errMsg,
				},
			},
		})
		_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
			SessionId: sessionID,
			Timestamp: timestamppb.Now(),
			Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
			Kind: &xrunnerv1.AgentEvent_Status{
				Status: &xrunnerv1.Status{Phase: "idle", Detail: ""},
			},
		})
	}()
}

func (s *sessionService) execToolSync(ctx context.Context, sessionID string, inv *xrunnerv1.ToolInvoke) (toolCallID string, content string, exit int32, errMsg string) {
	tool := strings.TrimSpace(inv.GetName())
	if tool == "" {
		return "", "", 2, "tool name is required"
	}

	bundle, bundlePath, bundleErr := s.bundleForSession(sessionID)
	if bundleErr != nil {
		return "", "", 2, fmt.Sprintf("tool bundle %q: %v", bundlePath, bundleErr)
	}
	if bundle != nil && !bundleAllows(bundle, tool) {
		return "", "", 2, "tool not permitted by bundle: " + tool
	}

	toolCallID = uuid.NewString()
	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
		Kind: &xrunnerv1.AgentEvent_ToolCallStarted{
			ToolCallStarted: &xrunnerv1.ToolCallStarted{
				ToolCallId: toolCallID,
				Name:       tool,
				ArgsJson:   inv.GetArgsJson(),
			},
		},
	})
	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
		Kind: &xrunnerv1.AgentEvent_Status{
			Status: &xrunnerv1.Status{Phase: "running_tool", Detail: tool},
		},
	})

	var out string
	switch tool {
	case "shell":
		exit, errMsg, out = s.execShellToolCapture(ctx, sessionID, toolCallID, inv.GetArgsJson())
	case "write_file_atomic":
		exit, errMsg, out = s.execWriteFileToolCapture(ctx, sessionID, toolCallID, inv.GetArgsJson())
	default:
		exit = 2
		errMsg = "unknown tool: " + tool
	}

	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
		Kind: &xrunnerv1.AgentEvent_ToolCallResult{
			ToolCallResult: &xrunnerv1.ToolCallResult{
				ToolCallId:   toolCallID,
				ExitCode:     exit,
				ErrorMessage: errMsg,
			},
		},
	})
	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_HIGH,
		Kind: &xrunnerv1.AgentEvent_Status{
			Status: &xrunnerv1.Status{Phase: "idle", Detail: ""},
		},
	})

	content = strings.TrimSpace(out)
	if errMsg != "" {
		if content != "" {
			content += "\n"
		}
		content += "error: " + errMsg
	}
	if content == "" {
		content = fmt.Sprintf("exit=%d", exit)
	} else {
		content = fmt.Sprintf("exit=%d\n%s", exit, content)
	}
	return toolCallID, content, exit, errMsg
}

func (s *sessionService) bundleForSession(sessionID string) (*toolspec.Bundle, string, error) {
	s.bundleMu.Lock()
	cached := s.bundles[sessionID]
	s.bundleMu.Unlock()
	if cached != nil {
		return cached, "", nil
	}

	sess, err := s.store.Get(sessionID)
	if err != nil {
		return nil, "", nil
	}
	path := strings.TrimSpace(sess.GetToolBundlePath())
	if path == "" {
		return nil, "", nil
	}
	b, err := toolspec.LoadBundle(path)
	if err != nil {
		return nil, path, err
	}
	s.bundleMu.Lock()
	s.bundles[sessionID] = b
	s.bundleMu.Unlock()
	return b, path, nil
}

func bundleAllows(b *toolspec.Bundle, name string) bool {
	if b == nil {
		return true
	}
	for _, t := range b.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

type shellArgs struct {
	Cmd string            `json:"cmd"`
	Cwd string            `json:"cwd"`
	Env map[string]string `json:"env"`
	PTY bool              `json:"pty"`
}

func (s *sessionService) resolveSessionWorkdir(sessionID, cwd string) (string, error) {
	sess, err := s.store.Get(sessionID)
	if err != nil {
		return "", fmt.Errorf("session not found")
	}
	root := strings.TrimSpace(sess.GetWorkspaceRoot())
	if root == "" {
		root = "/"
	}
	root = filepath.Clean(root)
	if strings.TrimSpace(cwd) == "" {
		return root, nil
	}
	if filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be relative")
	}
	abs := filepath.Clean(filepath.Join(root, cwd))
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("invalid cwd")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("cwd escapes workspace root")
	}
	return abs, nil
}

func (s *sessionService) execShellTool(ctx context.Context, sessionID, toolCallID, argsJSON string) (int32, string) {
	var args shellArgs
	if err := json.Unmarshal([]byte(defaultJSON(argsJSON, "{}")), &args); err != nil {
		return 2, "invalid args_json: " + err.Error()
	}
	cmdStr := strings.TrimSpace(args.Cmd)
	if cmdStr == "" {
		return 2, "cmd is required"
	}

	// Prefer PTY by default if not specified.
	usePTY := args.PTY
	if argsJSON == "" {
		usePTY = true
	}

	if usePTY {
		return s.execShellPTY(ctx, sessionID, toolCallID, cmdStr, args.Cwd, args.Env)
	}
	return s.execShellPipes(ctx, sessionID, toolCallID, cmdStr, args.Cwd, args.Env)
}

func (s *sessionService) execShellToolCapture(ctx context.Context, sessionID, toolCallID, argsJSON string) (int32, string, string) {
	var args shellArgs
	if err := json.Unmarshal([]byte(defaultJSON(argsJSON, "{}")), &args); err != nil {
		return 2, "invalid args_json: " + err.Error(), ""
	}
	cmdStr := strings.TrimSpace(args.Cmd)
	if cmdStr == "" {
		return 2, "cmd is required", ""
	}

	// Prefer PTY by default if not specified.
	usePTY := args.PTY
	if argsJSON == "" {
		usePTY = true
	}

	maxOut := s.llmCfg.ToolOutputMaxBytes
	if maxOut <= 0 {
		maxOut = 64 * 1024
	}

	if usePTY {
		buf := newLimitedBuffer(maxOut)
		exit, errMsg := s.execShellPTYTo(ctx, sessionID, toolCallID, cmdStr, args.Cwd, args.Env, buf)
		return exit, errMsg, buf.String()
	}

	stdoutBuf := newLimitedBuffer(maxOut / 2)
	stderrBuf := newLimitedBuffer(maxOut / 2)
	exit, errMsg := s.execShellPipesTo(ctx, sessionID, toolCallID, cmdStr, args.Cwd, args.Env, stdoutBuf, stderrBuf)
	var out strings.Builder
	if so := strings.TrimSpace(stdoutBuf.String()); so != "" {
		out.WriteString("[stdout]\n")
		out.WriteString(so)
		out.WriteString("\n")
	}
	if se := strings.TrimSpace(stderrBuf.String()); se != "" {
		out.WriteString("[stderr]\n")
		out.WriteString(se)
		out.WriteString("\n")
	}
	return exit, errMsg, out.String()
}

func (s *sessionService) execShellPipes(ctx context.Context, sessionID, toolCallID, cmdStr, cwd string, env map[string]string) (int32, string) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", cmdStr)
	dir, derr := s.resolveSessionWorkdir(sessionID, cwd)
	if derr != nil {
		return 2, derr.Error()
	}
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return 1, err.Error()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.streamReader(sessionID, toolCallID, "stdout", stdout)
	}()
	go func() {
		defer wg.Done()
		s.streamReader(sessionID, toolCallID, "stderr", stderr)
	}()

	err := cmd.Wait()
	wg.Wait()
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return int32(ee.ExitCode()), ""
	}
	return 1, err.Error()
}

func (s *sessionService) execShellPipesTo(ctx context.Context, sessionID, toolCallID, cmdStr, cwd string, env map[string]string, stdoutCap, stderrCap io.Writer) (int32, string) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", cmdStr)
	dir, derr := s.resolveSessionWorkdir(sessionID, cwd)
	if derr != nil {
		return 2, derr.Error()
	}
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return 1, err.Error()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.streamReaderTo(sessionID, toolCallID, "stdout", stdout, stdoutCap)
	}()
	go func() {
		defer wg.Done()
		s.streamReaderTo(sessionID, toolCallID, "stderr", stderr, stderrCap)
	}()

	err := cmd.Wait()
	wg.Wait()
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return int32(ee.ExitCode()), ""
	}
	return 1, err.Error()
}

func (s *sessionService) execShellPTY(ctx context.Context, sessionID, toolCallID, cmdStr, cwd string, env map[string]string) (int32, string) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", cmdStr)
	dir, derr := s.resolveSessionWorkdir(sessionID, cwd)
	if derr != nil {
		return 2, derr.Error()
	}
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	ws := &pty.Winsize{Cols: 120, Rows: 30}
	pt, err := startPTY(cmd, ws, true)
	if err != nil && strings.Contains(err.Error(), "Setctty set but Ctty not valid") {
		pt, err = startPTY(cmd, ws, false)
	}
	if err != nil {
		return 1, err.Error()
	}
	defer pt.Close()

	buf := make([]byte, 32*1024)
	for {
		_ = pt.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, rerr := pt.Read(buf)
		if n > 0 {
			_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
				SessionId: sessionID,
				Timestamp: timestamppb.Now(),
				Lane:      xrunnerv1.EventLane_EVENT_LANE_LOW,
				Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{
					ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
						ToolCallId: toolCallID,
						Stream:     "pty",
						Data:       append([]byte(nil), buf[:n]...),
					},
				},
			})
		}
		if rerr != nil {
			if errors.Is(rerr, os.ErrDeadlineExceeded) {
				if ctx.Err() != nil {
					_ = cmd.Process.Kill()
					break
				}
				continue
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			break
		}
	}

	err = cmd.Wait()
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return int32(ee.ExitCode()), ""
	}
	return 1, err.Error()
}

func (s *sessionService) execShellPTYTo(ctx context.Context, sessionID, toolCallID, cmdStr, cwd string, env map[string]string, cap io.Writer) (int32, string) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", cmdStr)
	dir, derr := s.resolveSessionWorkdir(sessionID, cwd)
	if derr != nil {
		return 2, derr.Error()
	}
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	ws := &pty.Winsize{Cols: 120, Rows: 30}
	pt, err := startPTY(cmd, ws, true)
	if err != nil && strings.Contains(err.Error(), "Setctty set but Ctty not valid") {
		pt, err = startPTY(cmd, ws, false)
	}
	if err != nil {
		return 1, err.Error()
	}
	defer pt.Close()

	buf := make([]byte, 32*1024)
	for {
		_ = pt.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, rerr := pt.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if cap != nil {
				_, _ = cap.Write(chunk)
			}
			_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
				SessionId: sessionID,
				Timestamp: timestamppb.Now(),
				Lane:      xrunnerv1.EventLane_EVENT_LANE_LOW,
				Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{
					ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
						ToolCallId: toolCallID,
						Stream:     "pty",
						Data:       chunk,
					},
				},
			})
		}
		if rerr != nil {
			if errors.Is(rerr, os.ErrDeadlineExceeded) {
				if ctx.Err() != nil {
					_ = cmd.Process.Kill()
					break
				}
				continue
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			break
		}
	}

	err = cmd.Wait()
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return int32(ee.ExitCode()), ""
	}
	return 1, err.Error()
}

func (s *sessionService) streamReader(sessionID, toolCallID, stream string, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
				SessionId: sessionID,
				Timestamp: timestamppb.Now(),
				Lane:      xrunnerv1.EventLane_EVENT_LANE_LOW,
				Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{
					ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
						ToolCallId: toolCallID,
						Stream:     stream,
						Data:       append([]byte(nil), buf[:n]...),
					},
				},
			})
		}
		if err != nil {
			return
		}
	}
}

func (s *sessionService) streamReaderTo(sessionID, toolCallID, stream string, r io.Reader, cap io.Writer) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if cap != nil {
				_, _ = cap.Write(chunk)
			}
			_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
				SessionId: sessionID,
				Timestamp: timestamppb.Now(),
				Lane:      xrunnerv1.EventLane_EVENT_LANE_LOW,
				Kind: &xrunnerv1.AgentEvent_ToolOutputChunk{
					ToolOutputChunk: &xrunnerv1.ToolOutputChunk{
						ToolCallId: toolCallID,
						Stream:     stream,
						Data:       chunk,
					},
				},
			})
		}
		if err != nil {
			return
		}
	}
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode"`
}

func (s *sessionService) execWriteFileTool(ctx context.Context, sessionID, toolCallID, argsJSON string) (int32, string) {
	exit, msg, _ := s.execWriteFileToolInternal(ctx, sessionID, toolCallID, argsJSON)
	return exit, msg
}

func (s *sessionService) execWriteFileToolCapture(ctx context.Context, sessionID, toolCallID, argsJSON string) (int32, string, string) {
	exit, msg, diff := s.execWriteFileToolInternal(ctx, sessionID, toolCallID, argsJSON)
	return exit, msg, diff
}

func (s *sessionService) execWriteFileToolInternal(ctx context.Context, sessionID, toolCallID, argsJSON string) (int32, string, string) {
	_ = ctx
	_ = toolCallID
	var args writeFileArgs
	if err := json.Unmarshal([]byte(defaultJSON(argsJSON, "{}")), &args); err != nil {
		return 2, "invalid args_json: " + err.Error(), ""
	}
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return 2, "path is required", ""
	}

	// Load session to resolve workspace root.
	sess, err := s.store.Get(sessionID)
	if err != nil {
		return 2, "session not found", ""
	}
	root := sess.GetWorkspaceRoot()
	if root == "" {
		root = "/"
	}
	if filepath.IsAbs(path) {
		return 2, "path must be relative", ""
	}
	root = filepath.Clean(root)
	abs := filepath.Clean(filepath.Join(root, path))
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return 2, "invalid path", ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return 2, "path escapes workspace root", ""
	}

	old, _ := os.ReadFile(abs)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 1, err.Error(), ""
	}
	mode := os.FileMode(args.Mode)
	if mode == 0 {
		mode = 0o644
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".xrunner-edit-*")
	if err != nil {
		return 1, err.Error(), ""
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return 1, err.Error(), ""
	}
	if _, err := tmp.WriteString(args.Content); err != nil {
		_ = tmp.Close()
		return 1, err.Error(), ""
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 1, err.Error(), ""
	}
	if err := tmp.Close(); err != nil {
		return 1, err.Error(), ""
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return 1, err.Error(), ""
	}

	diff := simpleTextDiff(string(old), args.Content)
	_, _ = s.appendAndBroadcast(&xrunnerv1.AgentEvent{
		SessionId: sessionID,
		Timestamp: timestamppb.Now(),
		Lane:      xrunnerv1.EventLane_EVENT_LANE_MEDIUM,
		Kind: &xrunnerv1.AgentEvent_FileDiff{
			FileDiff: &xrunnerv1.FileDiff{
				Path:        path,
				UnifiedDiff: diff,
			},
		},
	})
	return 0, "", diff
}

func (s *sessionService) appendAndBroadcast(evt *xrunnerv1.AgentEvent) (*xrunnerv1.AgentEvent, error) {
	appended, err := s.store.Append(evt)
	if err != nil {
		return nil, err
	}
	s.broadcast(appended)
	return appended, nil
}

func (s *sessionService) subscribe(sessionID string) (int64, *sessionSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subscribers[sessionID] == nil {
		s.subscribers[sessionID] = make(map[int64]*sessionSubscriber)
	}
	id := s.nextSubID
	s.nextSubID++
	sub := &sessionSubscriber{ch: make(chan *xrunnerv1.AgentEvent, 8192)}
	s.subscribers[sessionID][id] = sub
	return id, sub
}

func (s *sessionService) unsubscribe(sessionID string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.subscribers[sessionID]
	if subs == nil {
		return
	}
	if sub := subs[id]; sub != nil {
		delete(subs, id)
		close(sub.ch)
	}
	if len(subs) == 0 {
		delete(s.subscribers, sessionID)
	}
}

func (s *sessionService) broadcast(evt *xrunnerv1.AgentEvent) {
	s.mu.Lock()
	subs := s.subscribers[evt.GetSessionId()]
	targets := make([]*sessionSubscriber, 0, len(subs))
	for _, sub := range subs {
		targets = append(targets, sub)
	}
	s.mu.Unlock()
	for _, sub := range targets {
		sendDropOldest(sub.ch, evt)
	}
}

func sendDropOldest(ch chan *xrunnerv1.AgentEvent, evt *xrunnerv1.AgentEvent) {
	select {
	case ch <- evt:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- evt:
	default:
	}
}

func sendEvent(stream interface {
	Send(*xrunnerv1.AgentEvent) error
}, ev *xrunnerv1.AgentEvent, ok bool, after int64) error {
	if !ok {
		return io.EOF
	}
	if ev == nil {
		return nil
	}
	// Ephemeral events (event_id==0) are not persisted and must bypass replay filtering.
	if ev.GetEventId() > 0 && ev.GetEventId() <= after {
		return nil
	}
	return stream.Send(ev)
}

func defaultJSON(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func simpleTextDiff(oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	// Minimal diff suitable for terminal rendering without adding heavy dependencies.
	linesOld := strings.Split(oldText, "\n")
	linesNew := strings.Split(newText, "\n")
	var b strings.Builder
	b.WriteString("--- before\n+++ after\n")
	max := len(linesOld)
	if len(linesNew) > max {
		max = len(linesNew)
	}
	for i := 0; i < max; i++ {
		var o, n string
		if i < len(linesOld) {
			o = linesOld[i]
		}
		if i < len(linesNew) {
			n = linesNew[i]
		}
		if o == n {
			continue
		}
		if o != "" || i < len(linesOld)-1 {
			b.WriteString(fmt.Sprintf("-%s\n", o))
		}
		if n != "" || i < len(linesNew)-1 {
			b.WriteString(fmt.Sprintf("+%s\n", n))
		}
	}
	return b.String()
}
