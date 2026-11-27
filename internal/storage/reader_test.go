package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

func TestReaderDoesNotRecoverOrCreateFiles(t *testing.T) {
	path := t.TempDir()
	st, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := st.Save(ctx, key, fixture(t)); e != nil {
		t.Fatal(e)
	}
	st.Close()
	target := filepath.Join(path, "archive", key.Channel, key.RootTS)
	previous := filepath.Join(path, "archive", key.Channel, "."+key.RootTS+"-previous")
	if e := os.Rename(target, previous); e != nil {
		t.Fatal(e)
	}
	ro, e := OpenReader(path)
	if e != nil {
		t.Fatal(e)
	}
	defer ro.Close()
	v, e := ro.Read(context.Background(), key)
	if e != nil || v == nil {
		t.Fatal(v, e)
	}
	if _, e := os.Stat(target); !os.IsNotExist(e) {
		t.Fatal("reader recovered target")
	}
	if _, e := os.Stat(previous); e != nil {
		t.Fatal("reader removed previous", e)
	}
	if e := ro.Inspect(ctx, pluginapi.Key{Channel: "MISSING", RootTS: "100.000001"}, func(v *pluginapi.Snapshot) error {
		if v != nil {
			t.Fatal(v)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(path, "archive", "MISSING")); !os.IsNotExist(e) {
		t.Fatal("reader created channel")
	}
	if _, e := ro.Save(ctx, key, *v); e == nil {
		t.Fatal("reader can write")
	}
	if e := ro.RemoveArchive(ctx, key); e == nil {
		t.Fatal("reader can remove")
	}
	if _, e := OpenReader(filepath.Join(path, "absent")); e == nil {
		t.Fatal("accepted missing archive")
	}
	if _, e := os.Stat(filepath.Join(path, "absent")); !os.IsNotExist(e) {
		t.Fatal("reader created archive")
	}
}

func TestReaderWaitsForCapturePublication(t *testing.T) {
	path := t.TempDir()
	writer, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Close()
	if _, e := writer.Save(ctx, key, fixture(t)); e != nil {
		t.Fatal(e)
	}
	reader, e := OpenReader(path)
	if e != nil {
		t.Fatal(e)
	}
	defer reader.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writer.Catalog(ctx, func(CatalogVisitor) error {
			target := filepath.Join(path, "archive", key.Channel, key.RootTS)
			previous := filepath.Join(path, "archive", key.Channel, "."+key.RootTS+"-previous")
			if e := os.Rename(target, previous); e != nil {
				return e
			}
			close(entered)
			<-release
			return os.Rename(previous, target)
		})
	}()
	<-entered
	readDone := make(chan error, 1)
	go func() {
		v, e := reader.Read(ctx, key)
		if e == nil && v == nil {
			e = ErrIntegrity
		}
		readDone <- e
	}()
	select {
	case e := <-readDone:
		close(release)
		<-writeDone
		t.Fatal("reader entered publication gap", e)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if e := <-writeDone; e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-readDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not resume")
	}
}
