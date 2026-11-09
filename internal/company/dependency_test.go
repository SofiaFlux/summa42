package company_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnidirectionalDependencyRule strictly enforces Acceptance Criterion 1:
// Company packages may import Kernel packages, but Kernel packages must never import
// or reference Company packages.
func TestUnidirectionalDependencyRule(t *testing.T) {
	// List of canonical Kernel packages that must never depend on the Company profile layer
	kernelPackages := []string{
		"execution",
		"verification",
		"containment",
		"emergency",
		"capabilities",
		"policy",
		"experience",
		"quarantine",
		"workflow",
		"workflowcase",
		"state",
		"domain",
		"evidence",
		"modelrun",
		"runmanifest",
		"resources",
		"control",
		"capacity",
		"identity",
		"detection",
		"repoworkspace",
	}

	targetImport := "github.com/SofiaFlux/summa42/internal/company"
	filesChecked := 0

	for _, pkg := range kernelPackages {
		pkgDir := filepath.Join("..", pkg)
		err := filepath.WalkDir(pkgDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}

			if !strings.HasSuffix(path, ".go") {
				return nil
			}

			filesChecked++
			fset := token.NewFileSet()
			node, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if parseErr != nil {
				t.Fatalf("parse file %s: %v", path, parseErr)
			}

			for _, imp := range node.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				if importPath == targetImport || strings.HasPrefix(importPath, targetImport+"/") {
					t.Errorf("violation of unidirectional dependency rule: kernel file %s imports company package %s", path, importPath)
				}
			}

			return nil
		})

		if err != nil {
			t.Fatalf("walk kernel package %s: %v", pkg, err)
		}
	}

	if filesChecked == 0 {
		t.Fatal("expected to check kernel go files, checked 0")
	}
}
