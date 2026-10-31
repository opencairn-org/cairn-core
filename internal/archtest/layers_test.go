// Package archtest enforces the hexagonal layering (docs/adr/0002) on every
// non-test file under internal/.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/opencairn-org/cairn-core/"

// infra are driver libraries only secondary adapters (and the composition
// root) may use.
var infra = []string{"github.com/jackc/pgx", "github.com/nats-io/", "github.com/aws/"}

// rules: a file under the prefix must not import anything listed.
var rules = []struct {
	layer     string
	forbidden []string
}{
	{"internal/domain/", []string{module + "internal/port", module + "internal/usecase", module + "internal/adapter", module + "cmd/"}},
	{"internal/port/", []string{module + "internal/usecase", module + "internal/adapter", module + "cmd/"}},
	{"internal/usecase/", append([]string{module + "internal/adapter", module + "internal/db", module + "cmd/"}, infra...)},
	{"internal/adapter/primary/", append([]string{module + "internal/adapter/secondary", module + "internal/db", module + "cmd/"}, infra...)},
}

func TestLayering(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, r := range rules {
			if !strings.HasPrefix(rel, r.layer) {
				continue
			}
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range r.forbidden {
					if strings.HasPrefix(p, bad) {
						t.Errorf("%s imports %s (forbidden in %s)", rel, p, r.layer)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCompositionRoot: concrete secondary adapters are built only in
// cmd/server/wire.go (and the CLI commands that are their own roots).
func TestCompositionRoot(t *testing.T) {
	root := filepath.Join("..", "..", "cmd", "server")
	allowed := map[string]bool{"wire.go": true, "migrate.go": true, "rotate_key.go": true, "attachments_cmd.go": true, "serve.go": true}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "postgres.New") {
			t.Errorf("cmd/server/%s constructs a postgres adapter outside the composition root", name)
		}
	}
}
