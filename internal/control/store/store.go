package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

// Store keeps job and log state in memory while optionally mirroring changes to
// an external JetStream so state survives restarts.
type Store struct {
	mu          sync.RWMutex
	jobs        map[string]*xrunnerv1.Job
	logs        map[string][]*xrunnerv1.LogChunk
	logSubs     map[string]map[int64]chan *xrunnerv1.LogChunk
	nextSubID   int64
	jobVersions map[string]uint64
	logOffsets  map[string]uint64

	logger *slog.Logger
	js     *jetStreamMirror
}

// ErrJobNotFound marks when a job record is missing.
var ErrJobNotFound = fmt.Errorf("job not found")

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// New creates a Store with optional persistence options.
func New(ctx context.Context, opts *Options) (*Store, error) {
	logger := discardLogger
	if opts != nil && opts.Logger != nil {
		logger = opts.Logger
	}
	st := &Store{
		jobs:        make(map[string]*xrunnerv1.Job),
		logs:        make(map[string][]*xrunnerv1.LogChunk),
		logSubs:     make(map[string]map[int64]chan *xrunnerv1.LogChunk),
		jobVersions: make(map[string]uint64),
		logOffsets:  make(map[string]uint64),
		logger:      logger,
	}
	if opts != nil && opts.JetStream != nil {
		jsMirror, err := newJetStreamMirror(ctx, opts.JetStream, logger)
		if err != nil {
			return nil, err
		}
		if err := jsMirror.hydrate(ctx, st); err != nil {
			jsMirror.Close()
			return nil, err
		}
		st.js = jsMirror
	}
	return st, nil
}

// MustNew creates an in-memory Store and panics if initialization fails.
func MustNew() *Store {
	st, err := New(context.Background(), nil)
	if err != nil {
		panic(err)
	}
	return st
}

// Close flushes backing resources.
func (s *Store) Close() {
	if s.js != nil {
		s.js.Close()
	}
}

func compoundKey(ref *xrunnerv1.JobRef) string {
	if ref == nil {
		return ""
	}
	return fmt.Sprintf("%s:%s", ref.GetTenantId(), ref.GetJobId())
}

// Put upserts a job snapshot.
func (s *Store) Put(job *xrunnerv1.Job) {
	if job == nil {
		return
	}
	s.mu.Lock()
	key := compoundKey(job.GetRef())
	version := s.bumpJobVersionLocked(key)
	s.jobs[key] = proto.Clone(job).(*xrunnerv1.Job)
	s.mu.Unlock()
	if s.js != nil {
		if err := s.js.publishJob(job, version); err != nil {
			s.logger.Error("jetstream publish job", "job", key, "err", err)
		}
	}
}

// Get returns a copy of the job.
func (s *Store) Get(ref *xrunnerv1.JobRef) (*xrunnerv1.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[compoundKey(ref)]
	if !ok {
		return nil, ErrJobNotFound
	}
	return proto.Clone(job).(*xrunnerv1.Job), nil
}

// List returns all jobs for the tenant.
func (s *Store) List(tenantID string) []*xrunnerv1.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*xrunnerv1.Job, 0)
	for _, job := range s.jobs {
		if job.GetRef().GetTenantId() == tenantID {
			out = append(out, proto.Clone(job).(*xrunnerv1.Job))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i].GetCreateTime()
		right := out[j].GetCreateTime()
		if left == nil || right == nil {
			return out[i].GetRef().GetJobId() < out[j].GetRef().GetJobId()
		}
		return left.AsTime().Before(right.AsTime())
	})
	return out
}

// AppendLogs persists log chunks.
func (s *Store) AppendLogs(ref *xrunnerv1.JobRef, chunks []*xrunnerv1.LogChunk) {
	if ref == nil || len(chunks) == 0 {
		return
	}
	copied := protoSlicesCopy(chunks)
	s.mu.Lock()
	key := compoundKey(ref)
	offsets := make([]uint64, len(copied))
	for i := range copied {
		offsets[i] = s.bumpLogOffsetLocked(key)
		copied[i].Offset = offsets[i]
	}
	s.logs[key] = append(s.logs[key], copied...)
	s.broadcastLocked(key, copied)
	s.mu.Unlock()
	if s.js != nil {
		for i, chunk := range copied {
			if err := s.js.publishLog(ref, chunk, offsets[i]); err != nil {
				s.logger.Error("jetstream publish log", "job", key, "err", err)
			}
		}
	}
}

// Logs replays stored logs for the job.
func (s *Store) Logs(ref *xrunnerv1.JobRef) ([]*xrunnerv1.LogChunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := compoundKey(ref)
	chunks, ok := s.logs[key]
	if !ok {
		return nil, nil
	}
	return protoSlicesCopy(chunks), nil
}

func protoSlicesCopy(src []*xrunnerv1.LogChunk) []*xrunnerv1.LogChunk {
	out := make([]*xrunnerv1.LogChunk, 0, len(src))
	for _, chunk := range src {
		out = append(out, proto.Clone(chunk).(*xrunnerv1.LogChunk))
	}
	return out
}

// SubscribeLogs registers a log consumer for the given job.
func (s *Store) SubscribeLogs(ref *xrunnerv1.JobRef) (int64, <-chan *xrunnerv1.LogChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compoundKey(ref)
	ch := make(chan *xrunnerv1.LogChunk, 128)
	if s.logSubs[key] == nil {
		s.logSubs[key] = make(map[int64]chan *xrunnerv1.LogChunk)
	}
	id := s.nextSubID
	s.nextSubID++
	s.logSubs[key][id] = ch
	return id, ch
}

// UnsubscribeLogs removes a log consumer.
func (s *Store) UnsubscribeLogs(ref *xrunnerv1.JobRef, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compoundKey(ref)
	s.unsubscribeLocked(key, id)
}

// CloseLogStreams closes and removes all log subscribers for the job.
func (s *Store) CloseLogStreams(ref *xrunnerv1.JobRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compoundKey(ref)
	if subs := s.logSubs[key]; len(subs) > 0 {
		delete(s.logSubs, key)
		for _, ch := range subs {
			close(ch)
		}
	}
}

func (s *Store) unsubscribeLocked(key string, id int64) {
	subs := s.logSubs[key]
	if subs == nil {
		return
	}
	if ch, ok := subs[id]; ok {
		delete(subs, id)
		close(ch)
	}
	if len(subs) == 0 {
		delete(s.logSubs, key)
	}
}

func (s *Store) broadcastLocked(key string, chunks []*xrunnerv1.LogChunk) {
	if len(chunks) == 0 {
		return
	}
	subs := s.logSubs[key]
	if len(subs) == 0 {
		return
	}
	for _, ch := range subs {
		for _, chunk := range chunks {
			select {
			case ch <- proto.Clone(chunk).(*xrunnerv1.LogChunk):
			default:
			}
		}
	}
}

func (s *Store) applyReplayedJob(job *xrunnerv1.Job, version uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compoundKey(job.GetRef())
	s.jobs[key] = proto.Clone(job).(*xrunnerv1.Job)
	if version > s.jobVersions[key] {
		s.jobVersions[key] = version
	}
}

func (s *Store) applyReplayedLog(ref *xrunnerv1.JobRef, chunk *xrunnerv1.LogChunk, offset uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := compoundKey(ref)
	if offset > s.logOffsets[key] {
		s.logOffsets[key] = offset
	}
	copy := proto.Clone(chunk).(*xrunnerv1.LogChunk)
	copy.Offset = offset
	s.logs[key] = append(s.logs[key], copy)
	s.broadcastLocked(key, []*xrunnerv1.LogChunk{copy})
}

func (s *Store) bumpJobVersionLocked(key string) uint64 {
	s.jobVersions[key]++
	return s.jobVersions[key]
}

func (s *Store) bumpLogOffsetLocked(key string) uint64 {
	s.logOffsets[key]++
	return s.logOffsets[key]
}
