package logging

import (
	"bytes"
	"encoding/json"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationAndRedaction(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	sink, err := Open(dir, 180, 2, &stderr, slog.LevelDebug)
	if err != nil {
		t.Fatal(err)
	}
	logger := pluginapi.RedactingLogger(sink.Logger, pluginapi.Credentials{"token": "synthetic-secret-value"})
	for range 12 {
		logger.Info("step synthetic-secret-value", "cause", "synthetic-secret-value", "safe", "retained")
	}
	sink.Close()
	paths, _ := filepath.Glob(filepath.Join(dir, "logs", "chatkeeper.log*"))
	filtered := paths[:0]
	for _, p := range paths {
		if !strings.HasSuffix(p, ".lock") {
			filtered = append(filtered, p)
		}
	}
	paths = filtered
	if len(paths) != 3 {
		t.Fatalf("rotation: %v", paths)
	}
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(b, []byte("synthetic-secret-value")) {
			t.Fatal("secret leaked")
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if !json.Valid([]byte(line)) {
				t.Fatal("invalid JSON line", line)
			}
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	if strings.Contains(stderr.String(), "synthetic-secret-value") || !strings.Contains(stderr.String(), "retained") {
		t.Fatal("stderr redaction failed")
	}
}
func TestTailBoundAndFilter(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir, 100000, 2, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	for range 210 {
		s.Logger.Info("poll complete")
	}
	s.Logger.Error("archive failure")
	s.Close()
	lines, e := Tail(dir, "archive", 200)
	if e != nil || len(lines) != 1 || !strings.Contains(lines[0], "archive failure") {
		t.Fatal(lines, e)
	}
	lines, e = Tail(dir, "", 200)
	if e != nil || len(lines) != 200 {
		t.Fatal(len(lines), e)
	}
}

func TestConcurrentSinksReopenActiveRotation(t *testing.T) {
	dir := t.TempDir()
	a, e := Open(dir, 350, 20, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := Open(dir, 350, 20, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	for i := range 20 {
		a.Logger.Info("first writer", "sequence", i)
		b.Logger.Info("second writer", "sequence", i)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "logs", "chatkeeper.log*"))
	count := 0
	for _, p := range paths {
		if strings.HasSuffix(p, ".lock") {
			continue
		}
		raw, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			if !json.Valid(line) {
				t.Fatal("broken log record")
			}
			count++
		}
	}
	if count != 40 {
		t.Fatalf("lost records: got %d want 40", count)
	}
}

func TestAnotherWriterRotationDoesNotHideNewRecords(t *testing.T) {
	dir := t.TempDir()
	a, e := Open(dir, 180, 5, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := Open(dir, 180, 5, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	a.Logger.Info("before")
	for range 6 {
		b.Logger.Info("rotate this log")
	}
	a.Logger.Info("latest record")
	lines, e := Tail(dir, "latest record", 200)
	if e != nil || len(lines) != 1 {
		t.Fatal("new record hidden on rotated inode", lines, e)
	}
}
