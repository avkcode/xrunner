package sessionstore

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	xrunnerv1 "github.com/antonkrylov/xrunner/gen/go/xrunner/v1"
)

type Store struct {
	rootDir string

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	metaPath   string
	eventsPath string

	meta   *xrunnerv1.Session
	nextID int64

	file *os.File
	bw   *bufio.Writer

	lastSync time.Time
}

func New(rootDir string) *Store {
	return &Store{
		rootDir:  rootDir,
		sessions: make(map[string]*session),
	}
}

func (s *Store) Create(workspaceRoot, toolBundlePath string) (*xrunnerv1.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.NewString()
	dir := filepath.Join(s.rootDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	meta := &xrunnerv1.Session{
		SessionId:      id,
		WorkspaceRoot:  workspaceRoot,
		ToolBundlePath: toolBundlePath,
		CreatedAt:      timestamppb.New(time.Now()),
	}
	sess := &session{
		metaPath:   filepath.Join(dir, "session.pb"),
		eventsPath: filepath.Join(dir, "events.bin"),
		meta:       meta,
		nextID:     1,
	}
	if err := writeProtoFile(sess.metaPath, meta); err != nil {
		return nil, err
	}
	if err := sess.open(); err != nil {
		return nil, err
	}
	s.sessions[id] = sess
	return proto.Clone(meta).(*xrunnerv1.Session), nil
}

func (s *Store) List() ([]*xrunnerv1.Session, error) {
	entries, err := os.ReadDir(s.rootDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]*xrunnerv1.Session, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(s.rootDir, e.Name(), "session.pb")
		meta := &xrunnerv1.Session{}
		if err := readProtoFile(metaPath, meta); err != nil {
			continue
		}
		out = append(out, meta)
	}
	return out, nil
}

func (s *Store) Get(sessionID string) (*xrunnerv1.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.loadLocked(sessionID)
	if err != nil {
		return nil, err
	}
	return proto.Clone(sess.meta).(*xrunnerv1.Session), nil
}

func (s *Store) Delete(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[sessionID]; sess != nil {
		_ = sess.close()
		delete(s.sessions, sessionID)
	}
	return os.RemoveAll(filepath.Join(s.rootDir, sessionID))
}

func (s *Store) Append(evt *xrunnerv1.AgentEvent) (*xrunnerv1.AgentEvent, error) {
	if evt == nil {
		return nil, fmt.Errorf("event is nil")
	}
	if evt.GetSessionId() == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.loadLocked(evt.GetSessionId())
	if err != nil {
		return nil, err
	}
	copy := proto.Clone(evt).(*xrunnerv1.AgentEvent)
	copy.EventId = sess.nextID
	if copy.Timestamp == nil {
		copy.Timestamp = timestamppb.New(time.Now())
	}
	syncNow := copy.GetLane() != xrunnerv1.EventLane_EVENT_LANE_LOW
	sess.nextID++
	if err := sess.append(copy, syncNow); err != nil {
		return nil, err
	}
	return copy, nil
}

func (s *Store) Replay(sessionID string, afterEventID int64, send func(*xrunnerv1.AgentEvent) error) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	if send == nil {
		return fmt.Errorf("send is required")
	}
	s.mu.Lock()
	sess, err := s.loadLocked(sessionID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	path := sess.eventsPath
	s.mu.Unlock()

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		msg, err := readDelimited(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		ev := &xrunnerv1.AgentEvent{}
		if err := proto.Unmarshal(msg, ev); err != nil {
			return err
		}
		if ev.GetEventId() <= afterEventID {
			continue
		}
		if err := send(ev); err != nil {
			return err
		}
	}
}

func (s *Store) loadLocked(sessionID string) (*session, error) {
	if sess := s.sessions[sessionID]; sess != nil {
		return sess, nil
	}
	dir := filepath.Join(s.rootDir, sessionID)
	metaPath := filepath.Join(dir, "session.pb")
	meta := &xrunnerv1.Session{}
	if err := readProtoFile(metaPath, meta); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session not found")
		}
		return nil, err
	}
	sess := &session{
		metaPath:   metaPath,
		eventsPath: filepath.Join(dir, "events.bin"),
		meta:       meta,
	}
	if err := sess.open(); err != nil {
		return nil, err
	}
	s.sessions[sessionID] = sess
	return sess, nil
}

func (s *session) open() error {
	if err := os.MkdirAll(filepath.Dir(s.eventsPath), 0o755); err != nil {
		return err
	}
	// Determine nextID by scanning existing file.
	next, err := scanNextID(s.eventsPath)
	if err != nil {
		return err
	}
	s.nextID = next

	f, err := os.OpenFile(s.eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	s.bw = bufio.NewWriterSize(f, 256*1024)
	s.lastSync = time.Now()
	return nil
}

func (s *session) close() error {
	if s.bw != nil {
		_ = s.bw.Flush()
	}
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

func (s *session) append(evt *xrunnerv1.AgentEvent, syncNow bool) error {
	b, err := proto.Marshal(evt)
	if err != nil {
		return err
	}
	if err := writeDelimited(s.bw, b); err != nil {
		return err
	}
	if syncNow || time.Since(s.lastSync) > 200*time.Millisecond {
		if err := s.bw.Flush(); err != nil {
			return err
		}
		if err := s.file.Sync(); err != nil {
			return err
		}
		s.lastSync = time.Now()
		return nil
	}
	if s.bw.Buffered() > 256*1024 {
		return s.bw.Flush()
	}
	return nil
}

func scanNextID(path string) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var last int64
	for {
		msg, err := readDelimited(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		ev := &xrunnerv1.AgentEvent{}
		if err := proto.Unmarshal(msg, ev); err != nil {
			return 0, err
		}
		if ev.GetEventId() > last {
			last = ev.GetEventId()
		}
	}
	return last + 1, nil
}

func readDelimited(r *bufio.Reader) ([]byte, error) {
	l, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	if l == 0 {
		return nil, fmt.Errorf("invalid record length 0")
	}
	if l > 64*1024*1024 {
		return nil, fmt.Errorf("record too large: %d", l)
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeDelimited(w *bufio.Writer, msg []byte) error {
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(msg)))
	if _, err := w.Write(hdr[:n]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

func writeProtoFile(path string, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readProtoFile(path string, m proto.Message) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}
