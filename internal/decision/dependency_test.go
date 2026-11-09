package decision_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionUnidirectionalDependencyRule(t *testing.T) {
	internalDir := ".."
	targetImport := "github.com/SofiaFlux/summa42/internal/decision"
	filesChecked := 0

	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "decision" || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		fset := token.NewFileSet()
		node, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}

		filesChecked++
		for _, imp := range node.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == targetImport {
				t.Errorf("violation of decision layer isolation: internal file %s imports %s", path, targetImport)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed walking internal directory: %v", err)
	}

	if filesChecked < 50 {
		t.Fatalf("sanity check failed: only %d internal files were checked", filesChecked)
	}
}
