//go:build !windows

package storage

import (
	"context"
	"os"
	"syscall"
	"time"
)

func syncDirectory(f *os.File) error { return f.Sync() }
func lockFiles(ctx context.Context, s *Folder) (func(), error) {
	files := []*os.File{}
	unlock := func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(files[i].Fd()), syscall.LOCK_UN)
			_ = files[i].Close()
		}
	}
	for _, name := range []string{".", ".threadkeep.lock", ".chatkeeper.lock"} {

		if name != "." {
			if _, e := s.root.Lstat(name); e == nil {
				if e = s.safe(name); e != nil {
					unlock()
					return nil, e
				}
			}
		}
		flags := os.O_CREATE | os.O_RDWR
		mode := syscall.LOCK_EX
		if s.readOnly || name == "." {
			flags = os.O_RDONLY
		}
		if s.readOnly {
			mode = syscall.LOCK_SH
		}
		f, e := s.root.OpenFile(name, flags, 0600)
		if e != nil {
			if s.readOnly && os.IsNotExist(e) {
				continue
			}
			unlock()
			return nil, e
		}

		files = append(files, f)
		for {
			e = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
			if e == nil {
				break
			}
			if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
				unlock()
				return nil, e
			}
			select {
			case <-ctx.Done():
				unlock()
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return unlock, nil
}
