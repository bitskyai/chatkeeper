// Package logging provides bounded persistent diagnostics shared by CLI and UI.
package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const MaxSize int64 = 10 << 20
const Backups = 5

type Sink struct {
	Logger      *slog.Logger
	mu          sync.Mutex
	file        *os.File
	path        string
	size, limit int64
	backups     int
}

func Open(home string, limit int64, backups int, stderr io.Writer, level slog.Level) (*Sink, error) {
	if limit <= 0 || backups < 1 {
		return nil, errors.New("invalid log rotation limits")
	}
	dir := filepath.Join(home, "logs")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	s := &Sink{path: filepath.Join(dir, "chatkeeper.log"), limit: limit, backups: backups}
	if e := s.open(); e != nil {
		return nil, e
	}
	opts := &slog.HandlerOptions{Level: level}
	s.Logger = slog.New(tee{a: slog.NewJSONHandler(s, opts), b: slog.NewTextHandler(stderr, opts)})
	return s, nil
}
func (s *Sink) open() error {
	f, e := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	info, e := f.Stat()
	if e != nil {
		f.Close()
		return e
	}
	s.file = f
	s.size = info.Size()
	return nil
}
func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return 0, os.ErrClosed
	}
	coordination, e := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 0, e
	}
	defer coordination.Close()
	if e = lockFile(coordination); e != nil {
		return 0, e
	}
	defer unlockFile(coordination)
	// Another command may have rotated since our last write. Always reopen while locked.
	if e = s.file.Close(); e != nil {
		return 0, e
	}
	s.file = nil
	if e = s.open(); e != nil {
		return 0, e
	}
	if s.size > 0 && s.size+int64(len(p)) > s.limit {
		if e := s.file.Close(); e != nil {
			return 0, e
		}
		s.file = nil
		if e := os.Remove(fmt.Sprintf("%s.%d", s.path, s.backups)); e != nil && !os.IsNotExist(e) {
			return 0, e
		}
		for i := s.backups - 1; i >= 0; i-- {
			from := s.path
			if i > 0 {
				from = fmt.Sprintf("%s.%d", s.path, i)
			}
			if e := os.Rename(from, fmt.Sprintf("%s.%d", s.path, i+1)); e != nil && !os.IsNotExist(e) {
				return 0, e
			}
		}
		if e := s.open(); e != nil {
			return 0, e
		}
	}
	n, e := s.file.Write(p)
	s.size += int64(n)
	return n, e
}
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	e := s.file.Close()
	s.file = nil
	return e
}

type tee struct{ a, b slog.Handler }

func (h tee) Enabled(c context.Context, l slog.Level) bool {
	return h.a.Enabled(c, l) || h.b.Enabled(c, l)
}
func (h tee) Handle(c context.Context, r slog.Record) error {
	return errors.Join(h.a.Handle(c, r.Clone()), h.b.Handle(c, r))
}
func (h tee) WithAttrs(a []slog.Attr) slog.Handler { return tee{h.a.WithAttrs(a), h.b.WithAttrs(a)} }
func (h tee) WithGroup(n string) slog.Handler      { return tee{h.a.WithGroup(n), h.b.WithGroup(n)} }

// Tail reads only the active rotation, with a hard byte bound and up to 100000 lines.
func Tail(home, filter string, limit int) ([]string, error) {
	if limit < 1 || limit > 100000 {
		limit = 200
	}
	f, e := os.Open(filepath.Join(home, "logs", "chatkeeper.log"))
	if os.IsNotExist(e) {
		return []string{}, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	start := info.Size() - MaxSize
	if start < 0 {
		start = 0
	}
	if _, e = f.Seek(start, 0); e != nil {
		return nil, e
	}
	scanner := bufio.NewScanner(io.LimitReader(f, MaxSize))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	if start > 0 {
		scanner.Scan()
	}
	lines := make([]string, 0, limit)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(strings.ToLower(line), strings.ToLower(filter)) {
			continue
		}
		if len(lines) == limit {
			copy(lines, lines[1:])
			lines = lines[:limit-1]
		}
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}
