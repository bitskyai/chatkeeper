package config

import (
	"bytes"
	"errors"
	"go.yaml.in/yaml/v3"
	"os"
	"path/filepath"
	"sync"
)

var writeMu sync.Mutex

// AtomicWrite replaces a file only after its complete contents reach stable storage.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil && mode.Perm()&0077 == 0 {
		err = PrivatePath(f.Name())
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
func Writable(path string) bool {
	s, e := os.Stat(path)
	if os.IsNotExist(e) {
		s, e = os.Stat(filepath.Dir(path))
	}
	return e == nil && s.Mode().Perm()&0200 != 0
}

// Update patches one workspace using YAML nodes, retaining comments and key order.
func Update(path, id string, entry map[string]any) error {
	return UpdateValidated(path, id, entry, nil)
}

// UpdateValidated validates a private candidate and commits those exact bytes.
// All UI updates share the write lock through validation and rename.
func UpdateValidated(path, id string, entry map[string]any, validate func(string) error) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		b = []byte("workspaces: []\n")
	} else if e != nil {
		return e
	} else if !Writable(path) {
		return errors.New("managed configuration")
	}
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil || len(doc.Content) != 1 {
		return ErrConfig
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return ErrConfig
	}
	var seq *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "workspaces" {
			seq = root.Content[i+1]
		}
	}
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "workspaces"}, seq)
	}
	if seq.Kind != yaml.SequenceNode {
		return ErrConfig
	}
	if seq.LineComment != "" {
		seq.HeadComment += "\n" + seq.LineComment
		seq.LineComment = ""
	}
	seq.Style = 0
	var replacement yaml.Node
	if entry != nil {
		if replacement.Encode(entry) != nil {
			return ErrConfig
		}
	}
	found := false
	for i, n := range seq.Content {
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == "id" && n.Content[j+1].Value == id {
				found = true
				if entry == nil {
					seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
				} else {
					patchNode(n, &replacement)
					seq.Content[i] = n
				}
				break
			}
		}
		if found {
			break
		}
	}
	if !found && entry != nil {
		seq.Content = append(seq.Content, &replacement)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if enc.Encode(&doc) != nil {
		return ErrConfig
	}
	_ = enc.Close()
	if validate != nil {
		f, err := os.CreateTemp(filepath.Dir(path), ".validate-*")
		if err != nil {
			return err
		}
		candidate := f.Name()
		defer os.Remove(candidate)
		if _, err = f.Write(out.Bytes()); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err = validate(candidate); err != nil {
			return err
		}
		if _, err = os.Stat(path); err == nil {
			if err = AtomicWrite(path+".bak", b, 0600); err != nil {
				return err
			}
		}
		if err = os.Rename(candidate, path); err != nil {
			return err
		}
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
		return nil
	}
	if _, e = os.Stat(path); e == nil {
		if e = AtomicWrite(path+".bak", b, 0600); e != nil {
			return e
		}
	}
	return AtomicWrite(path, out.Bytes(), 0600)
}

// patchNode retains existing mapping keys and their comments while replacing values.
func patchNode(old, next *yaml.Node) {
	if old.Kind == yaml.MappingNode && next.Kind == yaml.MappingNode {
		kept := []*yaml.Node{}
		seen := map[string]bool{}
		for i := 0; i < len(old.Content); i += 2 {
			k, v := old.Content[i], old.Content[i+1]
			for j := 0; j < len(next.Content); j += 2 {
				if next.Content[j].Value == k.Value {
					patchNode(v, next.Content[j+1])
					kept = append(kept, k, v)
					seen[k.Value] = true
					break
				}
			}
		}
		for j := 0; j < len(next.Content); j += 2 {
			if !seen[next.Content[j].Value] {
				kept = append(kept, next.Content[j], next.Content[j+1])
			}
		}
		old.Content = kept
		return
	}
	head, line, foot := old.HeadComment, old.LineComment, old.FootComment
	*old = *next
	old.HeadComment = head
	old.LineComment = line
	old.FootComment = foot
}
