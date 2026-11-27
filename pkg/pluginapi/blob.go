package pluginapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Open rechecks size and digest on every use; the caller owns the returned handle.
// File references are opened through os.Root and all symlink components are refused.
func (b Blob) Open() (io.ReadCloser, error) {
	if b.Size < 0 || b.Size > 100<<20 || len(b.SHA256) != 64 {
		return nil, ErrInvalid
	}
	if b.Source == nil {
		h := sha256.Sum256(b.Data)
		if int64(len(b.Data)) != b.Size || hex.EncodeToString(h[:]) != b.SHA256 {
			return nil, ErrInvalid
		}
		return io.NopCloser(bytes.NewReader(b.Data)), nil
	}
	if !fs.ValidPath(b.Source.Path) {
		return nil, ErrInvalid
	}
	root, e := os.OpenRoot(b.Source.Root)
	if e != nil {
		return nil, ErrInvalid
	}
	defer root.Close()
	parts := strings.Split(b.Source.Path, "/")
	for i := range parts {
		info, e := root.Lstat(strings.Join(parts[:i+1], "/"))
		if e != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
	}
	f, e := root.Open(b.Source.Path)
	if e != nil {
		return nil, ErrInvalid
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() != b.Size {
		f.Close()
		return nil, ErrInvalid
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, b.Size+1))
	if e != nil || n != b.Size || hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
		f.Close()
		return nil, ErrInvalid
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		f.Close()
		return nil, ErrInvalid
	}
	return f, nil
}
