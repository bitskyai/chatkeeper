package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestPlatformBoundaries(t *testing.T) {
	root := "../.."
	banned := regexp.MustCompile(`(?i)(slack|discord|telegram)`)
	e := filepath.WalkDir(root, func(name string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".superpowers" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, name)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		inside := strings.HasPrefix(rel, "internal/plugins/")
		if strings.HasSuffix(name, ".go") {
			f, e := parser.ParseFile(token.NewFileSet(), name, nil, 0)
			if e != nil {
				return e
			}
			if !inside {
				ast.Inspect(f, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && banned.MatchString(id.Name) {
						t.Errorf("platform identifier outside plugin: %s: %s", rel, id.Name)
					}
					if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						value, _ := strconv.Unquote(lit.Value)
						if banned.MatchString(value) {
							t.Errorf("platform string outside plugin: %s", rel)
						}
					}
					return true
				})
			}
			for _, im := range f.Imports {
				value, _ := strconv.Unquote(im.Path.Value)
				if strings.Contains(value, "/internal/plugins/") {
					if strings.HasPrefix(rel, "cmd/") && strings.HasSuffix(value, "/internal/plugins/all") {
						continue
					}
					if strings.HasPrefix(rel, "internal/plugins/all/") {
						continue
					}
					if !inside {
						t.Errorf("plugin import outside composition root: %s", rel)
					} else {
						parts := strings.Split(rel, "/")
						own := "/internal/plugins/" + parts[2]
						if !strings.HasSuffix(value, own) && !strings.Contains(value, own+"/") {
							t.Errorf("cross-plugin import: %s", rel)
						}
					}
				}
				if inside && !strings.HasPrefix(rel, "internal/plugins/all/") && strings.Contains(value, "github.com/bitskyai/chatkeeper/internal/") && !strings.Contains(value, "/internal/plugins/") {
					t.Errorf("plugin imports host implementation: %s", rel)
				}
				if strings.HasPrefix(rel, "internal/core/") && strings.Contains(value, "/internal/cleanup") {
					t.Errorf("archive runtime imports cleanup")
				}
			}
		} else if strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".css") {
			if !inside {
				b, e := os.ReadFile(name)
				if e != nil {
					return e
				}
				if banned.Match(b) {
					t.Errorf("platform asset outside plugin: %s", rel)
				}
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
