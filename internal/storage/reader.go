package storage

import (
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
	"os"
)

// OpenReader opens an existing archive without creating directories or files.
// Readers wait for writers but never recover staging directories themselves.
func OpenReader(location string) (*Folder, error) {
	info, e := os.Lstat(location)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrIntegrity
	}
	root, e := os.OpenRoot(location)
	if e != nil {
		return nil, e
	}
	s := &Folder{path: location, root: root, failures: map[pluginapi.Key]Failure{}, readOnly: true}
	if e := s.safe("archive"); e != nil {
		root.Close()
		return nil, e
	}
	return s, nil
}
func (s *Folder) readerTarget(k pluginapi.Key) (string, error) {
	if !validKey(k) {
		return "", ErrIntegrity
	}
	target := "archive/" + k.Channel + "/" + k.RootTS
	if e := s.safe(target); e != nil {
		if !os.IsNotExist(e) {
			return "", e
		}
		previous := "archive/" + k.Channel + "/." + k.RootTS + "-previous"
		if e := s.safe(previous); e == nil {
			return previous, nil
		} else if !os.IsNotExist(e) {
			return "", e
		}
	}
	return target, nil
}
