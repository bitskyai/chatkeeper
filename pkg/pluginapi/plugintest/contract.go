// Package plugintest checks contract-v1 manifests, schemas and lifecycle behavior.
// Plugins run Check against their own synthetic transport fixture.
package plugintest

import (
	"context"
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"regexp"
	"runtime"
	"testing"
	"time"
)

func Check(t *testing.T, p pluginapi.Plugin, ws pluginapi.Workspace, env pluginapi.Env) {
	t.Helper()
	defer func() {
		if recover() != nil {
			t.Error("plugin contract call panicked")
		}
	}()
	m := p.Manifest()
	if e := pluginapi.ValidateManifest(m); e != nil {
		t.Fatal(e)
	}
	if m.APIVersion != pluginapi.APIVersion {
		t.Fatal("unsupported contract major")
	}
	for _, s := range [][]byte{p.ConfigSchema(), p.CredentialSchema()} {
		if e := pluginapi.ValidateSchema(s); e != nil {
			t.Fatal(e)
		}
	}
	seen := map[string]bool{}
	for _, step := range p.SetupFlow() {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(step.ID) || step.Title == "" || seen[step.ID] {
			t.Fatal("invalid setup step")
		}
		seen[step.ID] = true
		switch step.Kind {
		case "instructions", "form", "check", "choose":
		default:
			t.Fatal("invalid step kind")
		}
		if len(step.Schema) > 0 {
			if e := pluginapi.ValidateSchema(step.Schema); e != nil {
				t.Fatal(e)
			}
		}
	}
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, e := p.NewSession(ctx, ws, env)
	if e != nil {
		t.Fatal(e)
	}
	caps := session.Capabilities()
	if caps.ReadHistory && !m.Capabilities.ReadHistory || caps.Realtime && !m.Capabilities.Realtime || caps.Delete && !m.Capabilities.Delete {
		t.Fatal("session exceeds manifest capability ceiling")
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, time.Second)
	_, e = session.VerifyIdentity(checkCtx)
	checkCancel()
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	if caps.Realtime {
		go func() {
			var e error
			defer func() {
				if recover() != nil {
					e = errors.New("plugin Start panicked")
				}
				done <- e
			}()
			e = session.Start(ctx, func(pluginapi.Event) bool { return true })
		}()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case e := <-done:
			if e != nil && !errors.Is(e, context.Canceled) {
				t.Fatal("Start did not cancel cleanly")
			}
		case <-time.After(time.Second):
			t.Fatal("Start leaked after cancellation")
		}
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	stopped := make(chan error, 1)
	go func() {
		var e error
		defer func() {
			if recover() != nil {
				e = errors.New("stop panic")
			}
			stopped <- e
		}()
		e = session.Stop(stopCtx)
	}()
	select {
	case e := <-stopped:
		if e != nil {
			t.Fatal(e)
		}
	case <-stopCtx.Done():
		t.Fatal("Stop ignored deadline")
	}
	if !m.Capabilities.Delete {
		if _, e = p.NewCleanupDriver(context.Background(), ws, env); !errors.Is(e, pluginapi.ErrUnsupported) {
			t.Fatal("read-only plugin exposes cleanup")
		}
	}
	// Allow HTTP keepalive and test-server workers, but catch an unbounded lifecycle leak.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if runtime.NumGoroutine() > before+3 {
		t.Error("plugin leaked goroutines")
	}
}
