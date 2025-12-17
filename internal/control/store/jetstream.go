package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type jetStreamMirror struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	opts   *JetStreamOptions
	logger *slog.Logger
}

func newJetStreamMirror(ctx context.Context, opts *JetStreamOptions, logger *slog.Logger) (*jetStreamMirror, error) {
	cfg := *opts
	cfg.setDefaults()
	url := cfg.URL
	if url == "" {
		url = nats.DefaultURL
	}
	natsOpts := []nats.Option{nats.Name("xrunner-control-plane")}
	if cfg.User != "" {
		natsOpts = append(natsOpts, nats.UserInfo(cfg.User, cfg.Password))
	}
	conn, err := nats.Connect(url, natsOpts...)
	if err != nil {
		return nil, err
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, err
	}
	m := &jetStreamMirror{
		conn:   conn,
		js:     js,
		opts:   &cfg,
		logger: logger,
	}
	if err := m.ensureStreams(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return m, nil
}

func (m *jetStreamMirror) Close() {
	if m.conn != nil {
		m.conn.Drain()
		m.conn.Close()
	}
}

func (m *jetStreamMirror) ensureStreams(ctx context.Context) error {
	if err := m.ensureStream(ctx, &nats.StreamConfig{
		Name:       m.opts.JobsStream,
		Subjects:   []string{m.jobsWildcard()},
		Storage:    nats.FileStorage,
		Retention:  nats.LimitsPolicy,
		MaxMsgs:    -1,
		MaxBytes:   m.opts.JobsMaxBytes,
		Discard:    nats.DiscardOld,
		Duplicates: m.opts.DupeWindow,
	}); err != nil {
		return err
	}
	return m.ensureStream(ctx, &nats.StreamConfig{
		Name:       m.opts.LogsStream,
		Subjects:   []string{m.logsWildcard()},
		Storage:    nats.FileStorage,
		Retention:  nats.LimitsPolicy,
		MaxMsgs:    -1,
		MaxBytes:   m.opts.LogsMaxBytes,
		Discard:    nats.DiscardOld,
		Duplicates: m.opts.DupeWindow,
	})
}

func (m *jetStreamMirror) ensureStream(ctx context.Context, cfg *nats.StreamConfig) error {
	if _, err := m.js.StreamInfo(cfg.Name); err != nil {
		if errors.Is(err, nats.ErrStreamNotFound) {
			_, addErr := m.js.AddStream(cfg)
			return addErr
		}
		return err
	}
	_, err := m.js.UpdateStream(cfg)
	return err
}

func (m *jetStreamMirror) hydrate(ctx context.Context, st *Store) error {
	if err := m.replayJobs(ctx, st); err != nil {
		return err
	}
	return m.replayLogs(ctx, st)
}

func (m *jetStreamMirror) replayJobs(ctx context.Context, st *Store) error {
	sub, err := m.js.PullSubscribe(
		m.jobsWildcard(),
		"",
		nats.BindStream(m.opts.JobsStream),
		nats.DeliverAll(),
		nats.AckExplicit(),
	)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	return m.drain(ctx, sub, func(msg *nats.Msg) error {
		var evt xrunnerv1.JobEvent
		if err := proto.Unmarshal(msg.Data, &evt); err != nil {
			m.logger.Error("job replay decode", "err", err)
			return msg.Ack()
		}
		if evt.GetJob() != nil {
			st.applyReplayedJob(evt.GetJob(), evt.GetVersion())
		}
		return msg.Ack()
	})
}

func (m *jetStreamMirror) replayLogs(ctx context.Context, st *Store) error {
	sub, err := m.js.PullSubscribe(
		m.logsWildcard(),
		"",
		nats.BindStream(m.opts.LogsStream),
		nats.DeliverAll(),
		nats.AckExplicit(),
	)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	return m.drain(ctx, sub, func(msg *nats.Msg) error {
		var evt xrunnerv1.LogEvent
		if err := proto.Unmarshal(msg.Data, &evt); err != nil {
			m.logger.Error("log replay decode", "err", err)
			return msg.Ack()
		}
		if evt.GetRef() != nil && evt.GetChunk() != nil {
			st.applyReplayedLog(evt.GetRef(), evt.GetChunk(), evt.GetOffset())
		}
		return msg.Ack()
	})
}

func (m *jetStreamMirror) drain(ctx context.Context, sub *nats.Subscription, handler func(*nats.Msg) error) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msgs, err := sub.Fetch(64, nats.MaxWait(500*time.Millisecond))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return err
		}
		for _, msg := range msgs {
			if err := handler(msg); err != nil {
				return err
			}
		}
		if len(msgs) == 0 {
			return nil
		}
	}
}

func (m *jetStreamMirror) publishJob(job *xrunnerv1.Job, version uint64) error {
	if job == nil {
		return nil
	}
	payload, err := proto.Marshal(&xrunnerv1.JobEvent{
		Job:       proto.Clone(job).(*xrunnerv1.Job),
		Version:   version,
		EmittedAt: timestamppb.Now(),
	})
	if err != nil {
		return err
	}
	subject := m.jobSubject(job.GetRef())
	msgID := fmt.Sprintf("job:%s:%d", compoundKey(job.GetRef()), version)
	_, err = m.js.Publish(subject, payload, nats.MsgId(msgID))
	return err
}

func (m *jetStreamMirror) publishLog(ref *xrunnerv1.JobRef, chunk *xrunnerv1.LogChunk, offset uint64) error {
	if ref == nil || chunk == nil {
		return nil
	}
	payload, err := proto.Marshal(&xrunnerv1.LogEvent{
		Ref:       proto.Clone(ref).(*xrunnerv1.JobRef),
		Chunk:     proto.Clone(chunk).(*xrunnerv1.LogChunk),
		Offset:    offset,
		EmittedAt: timestamppb.Now(),
	})
	if err != nil {
		return err
	}
	subject := m.logSubject(ref)
	msgID := fmt.Sprintf("log:%s:%d", compoundKey(ref), offset)
	_, err = m.js.Publish(subject, payload, nats.MsgId(msgID))
	return err
}

func (m *jetStreamMirror) jobSubject(ref *xrunnerv1.JobRef) string {
	if ref == nil {
		return fmt.Sprintf("%s.jobs.unknown.unknown", m.opts.EventsPrefix)
	}
	return fmt.Sprintf("%s.jobs.%s.%s", m.opts.EventsPrefix, ref.GetTenantId(), ref.GetJobId())
}

func (m *jetStreamMirror) logSubject(ref *xrunnerv1.JobRef) string {
	if ref == nil {
		return fmt.Sprintf("%s.logs.unknown.unknown", m.opts.EventsPrefix)
	}
	return fmt.Sprintf("%s.logs.%s.%s", m.opts.EventsPrefix, ref.GetTenantId(), ref.GetJobId())
}

func (m *jetStreamMirror) jobsWildcard() string {
	return fmt.Sprintf("%s.jobs.*.*", m.opts.EventsPrefix)
}

func (m *jetStreamMirror) logsWildcard() string {
	return fmt.Sprintf("%s.logs.*.*", m.opts.EventsPrefix)
}
