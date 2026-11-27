package web_test

import (
	"bytes"
	"context"
	"github.com/bitskyai/chatkeeper/internal/logging"
	"github.com/bitskyai/chatkeeper/internal/web"
	"github.com/chromedp/chromedp"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBrowserLogs(t *testing.T) {
	if os.Getenv("CHATKEEPER_E2E") != "1" {
		t.Skip("set CHATKEEPER_E2E=1")
	}
	home := t.TempDir()
	sink, e := logging.Open(home, 100000, 2, &bytes.Buffer{}, slog.LevelInfo)
	if e != nil {
		t.Fatal(e)
	}
	defer sink.Close()
	sink.Logger.Info("<script>window.logXSS=true</script>", "step", "archive")
	a, e := web.NewApp(web.Options{Home: home, Mode: "desktop", Host: "127.0.0.1", LaunchToken: "synthetic-launch"})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	server := httptest.NewServer(a)
	defer server.Close()
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.NoSandbox)...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var text string
	var injected bool
	if e = chromedp.Run(ctx, chromedp.Navigate(server.URL+"/login?token=synthetic-launch"), chromedp.Navigate(server.URL+"/logs/?filter=archive"), chromedp.WaitVisible("pre"), chromedp.Text("pre", &text), chromedp.Evaluate("window.logXSS === true", &injected)); e != nil {
		t.Fatal(e)
	}
	if injected || !strings.Contains(text, "<script>") {
		t.Fatal("log text was not rendered safely", text)
	}
	sink.Logger.Info("auto-refresh-visible", "step", "archive")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var refreshed bool
		e = chromedp.Run(ctx, chromedp.Evaluate("document.querySelector('pre')?.textContent.includes('auto-refresh-visible') === true", &refreshed))
		if e == nil && refreshed {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("auto refresh did not expose the new log record")
}
