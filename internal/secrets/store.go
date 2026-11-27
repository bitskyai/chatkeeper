// Package secrets resolves write-only credentials without placing values in YAML.
package secrets

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitskyai/chatkeeper/internal/config"
	"github.com/zalando/go-keyring"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type Store struct {
	Home     string
	Desktop  bool
	mu       sync.Mutex
	keychain keychainBackend
}

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *Store) read() (map[string]string, error) {
	m := map[string]string{}
	p := filepath.Join(s.Home, "secrets.env")
	_, e := os.Stat(p)
	if os.IsNotExist(e) {
		return m, nil
	}
	if e != nil || !config.IsPrivate(p) {
		return nil, errors.New("unsafe secrets file")
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !namePattern.MatchString(k) {
			return nil, errors.New("invalid secrets file")
		}
		m[k] = v
	}
	return m, scan.Err()
}
func (s *Store) Get(name string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := os.Getenv(name); v != "" {
		return v, "environment", nil
	}
	backends, e := s.backends()
	if e != nil {
		return "", "", e
	}
	if s.Desktop && backends[name] != "file" {
		if v, e := s.keychainGet(name); e == nil {
			return v, "keychain", nil
		}
		if backends[name] == "keychain" {
			return "", "keychain", errors.New("keychain unavailable")
		}
	}
	m, e := s.read()
	return m[name], "file", e
}
func (s *Store) Set(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !namePattern.MatchString(name) || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("invalid credential")
	}
	if os.Getenv(name) != "" {
		return errors.New("credential provided by environment")
	}
	backends, e := s.backends()
	if e != nil {
		return e
	}
	if s.Desktop {
		_, existingError := s.keychainGet(name)
		if e := s.keychainSet(name, value); e == nil {
			backends[name] = "keychain"
			return s.saveBackends(backends)
		} else if backends[name] == "keychain" || existingError == nil {
			return errors.New("keychain rotation failed")
		}
	}

	m, e := s.read()
	if e != nil {
		return e
	}
	m[name] = value
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	if e := config.AtomicWrite(filepath.Join(s.Home, "secrets.env"), []byte(b.String()), 0600); e != nil {
		return e
	}
	backends[name] = "file"
	return s.saveBackends(backends)
}

func (s *Store) service() string {
	path, _ := filepath.Abs(s.Home)
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("chatkeeper-%x", sum[:12])
}

type keychainBackend interface {
	Get(string, string) (string, error)
	Set(string, string, string) error
}

func (s *Store) keychainGet(name string) (string, error) {
	if s.keychain != nil {
		return s.keychain.Get(s.service(), name)
	}
	return keyring.Get(s.service(), name)
}
func (s *Store) keychainSet(name, value string) error {
	if s.keychain != nil {
		return s.keychain.Set(s.service(), name, value)
	}
	return keyring.Set(s.service(), name, value)
}
func (s *Store) backends() (map[string]string, error) {
	m := map[string]string{}
	b, e := os.ReadFile(filepath.Join(s.Home, "secrets.backends.json"))
	if os.IsNotExist(e) {
		return m, nil
	}
	if e != nil {
		return nil, e
	}
	if len(b) > 1<<20 || json.Unmarshal(b, &m) != nil {
		return nil, errors.New("invalid secret backend metadata")
	}
	return m, nil
}
func (s *Store) saveBackends(m map[string]string) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	return config.AtomicWrite(filepath.Join(s.Home, "secrets.backends.json"), b, 0600)
}

// WatchPath identifies the file-backed credential change boundary.
func (s *Store) WatchPath() string { return filepath.Join(s.Home, "secrets.env") }

// Delete removes a locally stored credential; environment credentials are untouched.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !namePattern.MatchString(name) {
		return errors.New("invalid credential")
	}
	backends, err := s.backends()
	if err != nil {
		return err
	}
	if backends[name] == "keychain" {
		if err := keyring.Delete(s.service(), name); err != nil && err != keyring.ErrNotFound {
			return err
		}
	}
	m, err := s.read()
	if err != nil {
		return err
	}
	delete(m, name)
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	if err := config.AtomicWrite(filepath.Join(s.Home, "secrets.env"), []byte(b.String()), 0600); err != nil {
		return err
	}
	delete(backends, name)
	return s.saveBackends(backends)
}

// Prune removes superseded local drafts, preserving current policy and form refs.
func (s *Store) Prune(prefix string, keep ...string) error {
	s.mu.Lock()
	refs, err := s.backends()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for ref := range refs {
		if !strings.HasPrefix(ref, prefix) {
			continue
		}
		preserve := false
		for _, k := range keep {
			if ref == k {
				preserve = true
			}
		}
		if !preserve {
			if err := s.Delete(ref); err != nil {
				return err
			}
		}
	}
	return nil
}
