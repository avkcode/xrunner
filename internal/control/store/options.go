package store

import (
	"log/slog"
	"time"
)

// Options configure the store.
type Options struct {
	Logger    *slog.Logger
	JetStream *JetStreamOptions
}

// JetStreamOptions describe how to persist history in NATS JetStream.
type JetStreamOptions struct {
	URL           string
	User          string
	Password      string
	EventsPrefix  string
	JobsStream    string
	LogsStream    string
	DurablePrefix string
	JobsMaxBytes  int64
	LogsMaxBytes  int64
	DupeWindow    time.Duration
}

func (o *JetStreamOptions) setDefaults() {
	if o.EventsPrefix == "" {
		o.EventsPrefix = "events"
	}
	if o.JobsStream == "" {
		o.JobsStream = "xrunner_jobs"
	}
	if o.LogsStream == "" {
		o.LogsStream = "xrunner_logs"
	}
	if o.DurablePrefix == "" {
		o.DurablePrefix = "xrunner-replayer"
	}
	if o.JobsMaxBytes == 0 {
		o.JobsMaxBytes = 10 * 1024 * 1024 * 1024 // 10GB
	}
	if o.LogsMaxBytes == 0 {
		o.LogsMaxBytes = 200 * 1024 * 1024 * 1024 // 200GB
	}
	if o.DupeWindow == 0 {
		o.DupeWindow = 2 * time.Minute
	}
}
