package main

import (
	"path/filepath"
	"testing"
)

func TestInstanceLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "instance.lock")
	unlock, e := instanceLock(p)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := instanceLock(p); e == nil {
		second()
		t.Fatal("two instance owners")
	}
	unlock()
	third, e := instanceLock(p)
	if e != nil {
		t.Fatal(e)
	}
	third()
}
