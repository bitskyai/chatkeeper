package web

import (
	"bytes"
	"github.com/bitskyai/chatkeeper/internal/logging"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogsPageEscapesAndDownloads(t *testing.T) {
	home := t.TempDir()
	sink, e := logging.Open(home, 100000, 2, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	defer sink.Close()
	sink.Logger.Error("<script>alert(1)</script>", "step", "archive")
	a := &App{opts: Options{Home: home}}
	w := httptest.NewRecorder()
	a.logsPage(w, httptest.NewRequest("GET", "/logs/?filter=archive", nil), "csrf")
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>alert") || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.logsPage(w, httptest.NewRequest("GET", "/logs/?download=1", nil), "csrf")
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(w.Body.String(), "archive") {
		t.Fatal(w.Header(), w.Body.String())
	}
}
