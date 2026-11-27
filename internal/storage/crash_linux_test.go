package storage

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A real subprocess is killed after filesystem notifications at publication
// boundaries. No production failpoints or graceful cleanup run in the child.
func TestCrashRecoveryProcess(t *testing.T) {
	if dir := os.Getenv("CHATKEEPER_CRASH_CHILD"); dir != "" {
		s, e := Open(dir)
		if e != nil {
			t.Fatal(e)
		}
		v := fixture(t)
		v.Documents = nil
		v.Messages[0]["text"] = "new generation"
		if _, e = s.Save(context.Background(), key, v); e != nil {
			t.Fatal(e)
		}
		select {}
	}
	for _, boundary := range []string{"stage", "previous", "published"} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			s, e := Open(dir)
			if e != nil {
				t.Fatal(e)
			}
			old := fixture(t)
			if _, e = s.Save(ctx, key, old); e != nil {
				t.Fatal(e)
			}
			s.Close()
			parent := filepath.Join(dir, "archive", key.Channel)
			fd, e := syscall.InotifyInit1(syscall.IN_CLOEXEC)
			if e != nil {
				t.Fatal(e)
			}
			defer syscall.Close(fd)
			if _, e = syscall.InotifyAddWatch(fd, parent, syscall.IN_CREATE|syscall.IN_MOVED_TO); e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecoveryProcess$")
			cmd.Env = append(os.Environ(), "CHATKEEPER_CRASH_CHILD="+dir)
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			seen := make(chan bool, 1)
			go func() {
				buf := make([]byte, 4096)
				for {
					n, e := syscall.Read(fd, buf)
					if e != nil {
						seen <- false
						return
					}
					for pos := 0; pos+16 <= n; {
						mask := binary.NativeEndian.Uint32(buf[pos+4:])
						length := int(binary.NativeEndian.Uint32(buf[pos+12:]))
						if pos+16+length > n {
							seen <- false
							return
						}
						name := strings.TrimRight(string(buf[pos+16:pos+16+length]), "\x00")
						pos += 16 + length
						if boundary == "stage" && mask&syscall.IN_CREATE != 0 && strings.Contains(name, "-next-") || boundary == "previous" && name == "."+key.RootTS+"-previous" || boundary == "published" && name == key.RootTS && mask&syscall.IN_MOVED_TO != 0 {
							_ = cmd.Process.Signal(syscall.SIGSTOP)
							_ = cmd.Process.Kill()
							seen <- true
							return
						}
					}
				}
			}()
			select {
			case ok := <-seen:
				if !ok {
					t.Fatal("watch failed")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("publication boundary not reached")
			}
			_ = cmd.Wait()
			recovered, e := Open(dir)
			if e != nil {
				t.Fatal(e)
			}
			defer recovered.Close()
			v, e := recovered.Read(ctx, key)
			if e != nil || v == nil {
				t.Fatal("mixed/unrecoverable generation", e)
			}
			text, _ := v.Messages[0]["text"].(string)
			if text != "new generation" && text != old.Messages[0]["text"] {
				t.Fatal(text)
			}
			if _, e = v.Blobs["files/FFILE.bin"].Open(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
