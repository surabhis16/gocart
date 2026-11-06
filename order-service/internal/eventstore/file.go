package eventstore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gocart/order-service/internal/domain"
)

// File is an append-only event log: one JSON object per line, never rewritten.
//
// Durability: every Append is fsynced before it returns, so an acknowledged
// event survives a crash. A crash during a write can leave a half-written last
// line; Open detects that and cuts it off. A bad line anywhere else cannot be
// explained by a crash, so Open refuses to start rather than guess.
//
// A File is for a single writer process. Two processes appending to one file
// would interleave versions; that is what the Cassandra store is for.
type File struct {
	mu      sync.RWMutex
	f       *os.File // nil for a memory-only store
	closed  bool
	size    int64
	streams map[string][]domain.Event
	all     []domain.Event
	touched map[string]time.Time // when this process last appended to each stream

	// RepairedBytes is how much of a torn final write Open discarded.
	RepairedBytes int64
}

// OpenFile opens (or creates) dir/events.log and replays it into memory. An
// empty dir gives a memory-only store for tests.
func OpenFile(dir string) (*File, error) {
	s := &File{streams: map[string][]domain.Event{}, touched: map[string]time.Time{}}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dir, "events.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	if err := s.load(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	s.f = f
	return s, nil
}

func (s *File) load(f *os.File, path string) error {
	r := bufio.NewReader(f)
	var offset int64 // end of the last complete, valid line
	line := 0
	for {
		b, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(b) > 0 {
				// Bytes with no trailing newline: the process died mid-write.
				// The write was never acknowledged, so discarding it is correct.
				s.RepairedBytes = int64(len(b))
				if err := f.Truncate(offset); err != nil {
					return fmt.Errorf("repair torn write in %s: %w", path, err)
				}
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		line++
		var e domain.Event
		if err := json.Unmarshal(b, &e); err != nil {
			return fmt.Errorf("%s line %d is corrupt (%v); refusing to start, since skipping it would silently change order history", path, line, err)
		}
		if want := len(s.streams[e.StreamID]) + 1; e.Version != want {
			return fmt.Errorf("%s line %d: stream %s has version %d where %d was expected; the log is damaged", path, line, e.StreamID, e.Version, want)
		}
		s.streams[e.StreamID] = append(s.streams[e.StreamID], e)
		s.all = append(s.all, e)
		offset += int64(len(b))
	}
	s.size = offset
	return nil
}

// Append implements Store.
func (s *File) Append(ctx context.Context, e domain.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.StreamID == "" || e.Version < 1 {
		return fmt.Errorf("eventstore: event needs a stream id and a version of at least 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return errors.New("eventstore: store is closed")
	}
	if have := len(s.streams[e.StreamID]); e.Version != have+1 {
		return fmt.Errorf("%w: stream %s has %d events, tried to write version %d", ErrVersionConflict, e.StreamID, have, e.Version)
	}
	if s.f != nil {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if _, err := s.f.Write(b); err != nil {
			// Put the file back to its last good length so a partial line does
			// not poison the next append.
			_ = s.f.Truncate(s.size)
			return fmt.Errorf("append to event log: %w", err)
		}
		if err := s.f.Sync(); err != nil {
			_ = s.f.Truncate(s.size)
			return fmt.Errorf("fsync event log: %w", err)
		}
		s.size += int64(len(b))
	}
	s.streams[e.StreamID] = append(s.streams[e.StreamID], e)
	s.all = append(s.all, e)
	s.touched[e.StreamID] = time.Now()
	return nil
}

// ChangedSince implements Store. A file has one writer process, so "changed"
// means appended by this process since it started; anything older was read at
// startup and is already in every reader's full load.
func (s *File) ChangedSince(ctx context.Context, since time.Time) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for id, at := range s.touched {
		if !at.Before(since) {
			out = append(out, id)
		}
	}
	return out, nil
}

// Load implements Store.
func (s *File) Load(ctx context.Context, streamID string) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Event(nil), s.streams[streamID]...), nil
}

// LoadAll implements Store.
func (s *File) LoadAll(ctx context.Context) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Event(nil), s.all...), nil
}

// Close implements Store.
func (s *File) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
