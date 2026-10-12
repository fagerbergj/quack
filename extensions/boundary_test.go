// Package extensions holds only this test; each extension is its own package below it.
package extensions

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	quackPath = "github.com/fagerbergj/quack/"
	extPath   = quackPath + "extensions/"
	sdkPath   = extPath + "sdk"
	// The one core file allowed to name an extension, and only as a blank import that runs its sdk.Register.
	registryFile = "internal/serve/extensions_registry.go"
)

// imports maps each .go file under root (repo-relative) to its import specs, name ("_" for blank) by path.
func imports(t *testing.T, root string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	err := filepath.WalkDir(filepath.Join("..", root), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("..", p)
		specs := map[string]string{}
		for _, s := range f.Imports {
			path, _ := strconv.Unquote(s.Path.Value)
			specs[path] = ""
			if s.Name != nil {
				specs[path] = s.Name.Name
			}
		}
		out[filepath.ToSlash(rel)] = specs
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Extensions reach core only through the sdk, so the sdk stays the whole contract an outside extension needs.
func TestExtensionsImportCoreOnlyThroughSDK(t *testing.T) {
	for file, specs := range imports(t, "extensions") {
		for path := range specs {
			if strings.HasPrefix(path, quackPath) && !strings.HasPrefix(path, extPath) {
				t.Errorf("%s imports %s; extensions may reach core only through %s", file, path, sdkPath)
			}
		}
	}
}

// Core code links extensions only by blank import; tests may name one to check core's policy against it.
func TestCoreImportsExtensionsOnlyToRegister(t *testing.T) {
	roots := []string{"internal", "cmd", "tools"}
	top, _ := filepath.Glob(filepath.Join("..", "*.go"))
	for _, f := range top {
		roots = append(roots, filepath.Base(f)) // module-root files such as embed.go
	}
	for _, root := range roots {
		for file, specs := range imports(t, root) {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			for path, name := range specs {
				if !strings.HasPrefix(path, extPath) || path == sdkPath || strings.HasPrefix(path, sdkPath+"/") {
					continue
				}
				if file != registryFile || name != "_" {
					t.Errorf("%s imports %s; only %s may, as a blank import", file, path, registryFile)
				}
			}
		}
	}
}
