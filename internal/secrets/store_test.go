package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileAndEnvironment(t *testing.T) {
	s := Store{Home: t.TempDir()}
	if e := s.Set("TEST_CREDENTIAL", "synthetic-value"); e != nil {
		t.Fatal(e)
	}
	v, src, e := s.Get("TEST_CREDENTIAL")
	if e != nil || v != "synthetic-value" || src != "file" {
		t.Fatal(src, e)
	}
	t.Setenv("TEST_CREDENTIAL", "override")
	v, src, e = s.Get("TEST_CREDENTIAL")
	if e != nil || v != "override" || src != "environment" {
		t.Fatal(src, e)
	}
	if runtime.GOOS == "windows" {
		return
	}
	_ = os.Chmod(filepath.Join(s.Home, "secrets.env"), 0644)
	t.Setenv("TEST_CREDENTIAL", "")
	if _, _, e = s.Get("TEST_CREDENTIAL"); e == nil {
		t.Fatal("accepted public secrets")
	}
}

type unstableKeychain struct {
	value string
	fail  bool
}

func (k *unstableKeychain) Get(string, string) (string, error) {
	if k.fail || k.value == "" {
		return "", errors.New("unavailable")
	}
	return k.value, nil
}
func (k *unstableKeychain) Set(_, _, v string) error {
	if k.fail {
		return errors.New("unavailable")
	}
	k.value = v
	return nil
}
func TestKeychainRotationNeverResurrectsOldSecret(t *testing.T) {
	k := &unstableKeychain{}
	s := Store{Home: t.TempDir(), Desktop: true, keychain: k}
	if e := s.Set("TEST_KEY", "first"); e != nil {
		t.Fatal(e)
	}
	k.fail = true
	if s.Set("TEST_KEY", "second") == nil {
		t.Fatal("rotation silently fell back behind stale keychain value")
	}
	k.fail = false
	v, _, e := s.Get("TEST_KEY")
	if e != nil || v != "first" {
		t.Fatal(v, e)
	}
}
func TestFallbackRemainsAuthoritativeAfterKeychainRecovery(t *testing.T) {
	k := &unstableKeychain{value: "stale", fail: true}
	s := Store{Home: t.TempDir(), Desktop: true, keychain: k}
	if e := s.Set("TEST_KEY", "replacement"); e != nil {
		t.Fatal(e)
	}
	k.fail = false
	v, source, e := s.Get("TEST_KEY")
	if e != nil || v != "replacement" || source != "file" {
		t.Fatal("fallback resurrected old value", source, e)
	}
}

func TestPruneSupersededDrafts(t *testing.T) {
	s := Store{Home: t.TempDir()}
	for _, ref := range []string{"TEST_DRAFT_OLD", "TEST_DRAFT_ACTIVE", "TEST_DRAFT_CURRENT", "OTHER"} {
		if err := s.Set(ref, "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune("TEST_DRAFT_", "TEST_DRAFT_ACTIVE", "TEST_DRAFT_CURRENT"); err != nil {
		t.Fatal(err)
	}
	v, _, err := s.Get("TEST_DRAFT_OLD")
	if err != nil || v != "" {
		t.Fatal("superseded secret retained", err)
	}
	for _, ref := range []string{"TEST_DRAFT_ACTIVE", "TEST_DRAFT_CURRENT", "OTHER"} {
		v, _, err := s.Get(ref)
		if err != nil || v != "synthetic" {
			t.Fatal("preserved ref lost", ref, err)
		}
	}
}
