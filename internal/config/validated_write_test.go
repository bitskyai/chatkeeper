package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidatedUpdateCommitsOnlyCandidate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("# fixture comment\nworkspaces: []\n")
	if e := os.WriteFile(p, original, 0600); e != nil {
		t.Fatal(e)
	}
	var validated []byte
	e := UpdateValidated(p, "fixture", map[string]any{"id": "fixture", "platform": "fixture"}, func(candidate string) error {
		var err error
		validated, err = os.ReadFile(candidate)
		if err != nil {
			return err
		}
		// An external writer changes the source during validation. Never patch and
		// publish this unvalidated source after validating the candidate.
		return os.WriteFile(p, []byte("invalid: true\n"), 0600)
	})
	if e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if string(got) != string(validated) {
		t.Fatal("committed bytes differed from validated candidate")
	}
	before := string(got)
	if e = UpdateValidated(p, "fixture", nil, func(string) error { return errors.New("invalid candidate") }); e == nil {
		t.Fatal("accepted invalid candidate")
	}
	got, _ = os.ReadFile(p)
	if string(got) != before {
		t.Fatal("failed validation modified source")
	}
}
