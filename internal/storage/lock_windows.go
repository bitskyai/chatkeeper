package storage

import (
	"context"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var lockFileEx = kernel.NewProc("LockFileEx")
var unlockFileEx = kernel.NewProc("UnlockFileEx")

// Windows cannot fsync directory handles; file FlushFileBuffers plus atomic rename
// provides process-crash recovery. Power-loss durability depends on the filesystem.
func syncDirectory(_ *os.File) error { return nil }
func lockFiles(ctx context.Context, s *Folder, extra ...string) (func(), error) {
	files := []*os.File{}
	ovs := []*syscall.Overlapped{}
	unlock := func() {
		for i := len(files) - 1; i >= 0; i-- {
			_, _, _ = unlockFileEx.Call(files[i].Fd(), 0, 1, 0, uintptr(unsafe.Pointer(ovs[i])))
			_ = files[i].Close()
		}
	}
	for _, name := range append(extra, ".threadkeep.lock", ".chatkeeper.lock") {
		if _, e := s.root.Lstat(name); e == nil {
			if e = s.safe(name); e != nil {
				unlock()
				return nil, e
			}
		}
		flags := os.O_CREATE | os.O_RDWR
		lockFlags := uintptr(3)
		if s.readOnly {
			flags = os.O_RDONLY
			lockFlags = 1
		}
		f, e := s.root.OpenFile(name, flags, 0600)
		if e != nil {
			if s.readOnly && os.IsNotExist(e) {
				continue
			}
			unlock()
			return nil, e
		}
		ov := new(syscall.Overlapped)
		for {
			ok, _, err := lockFileEx.Call(f.Fd(), lockFlags, 0, 1, 0, uintptr(unsafe.Pointer(ov)))
			if ok != 0 {
				break
			}
			if err != syscall.Errno(33) {
				f.Close()
				unlock()
				return nil, err
			}
			select {
			case <-ctx.Done():
				f.Close()
				unlock()
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		files = append(files, f)
		ovs = append(ovs, ov)
	}
	return unlock, nil
}
