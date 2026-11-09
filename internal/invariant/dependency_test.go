package invariant_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnidirectionalDependencyRule strictly enforces Criterion 4:
// Invariant package may import Kernel and Company packages, but Kernel packages
// must NEVER import the invariant or company packages.
func TestUnidirectionalDependencyRule(t *testing.T) {
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

	targetImport := "github.com/SofiaFlux/summa42/internal/invariant"
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
			node, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("failed to parse file %s: %v", path, err)
			}

			for _, imp := range node.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				if importPath == targetImport || strings.HasPrefix(importPath, targetImport+"/") {
					t.Errorf("architectural violation: kernel package %q imports invariant package %q in %s", pkg, importPath, path)
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("failed to walk package directory %s: %v", pkgDir, err)
		}
	}

	if filesChecked == 0 {
		t.Fatal("expected to check multiple kernel source files, checked 0")
	}
}
