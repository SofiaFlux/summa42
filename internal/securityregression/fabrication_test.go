package securityregression_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fabrication tripwire (development plan, rule R4 and card M0-T6).
//
// On 2026-10-21 reviews were found to be produced by a canned adapter
// (docs/incidents/2026-10-21-canned-model-reviews.md). This test fails when a
// non-test Go file under internal/ or cmd/ has a function named Evaluate,
// Review, Assess or Verdict whose whole body is one return statement made
// only of literals, at least one of them a string: a verdict nobody computed.
//
// This is a tripwire, not a proof. It catches the laziest form of a
// fabricated verdict. It does not catch a constant hidden behind a variable,
// a condition that is always true, or a function with another name. The
// control is review of the code; this only makes one known shape loud.

var verdictFunctionNames = map[string]bool{
	"Evaluate": true,
	"Review":   true,
	"Assess":   true,
	"Verdict":  true,
}

// literalOnly reports whether the expression is built from literals alone,
// and whether a string literal occurs in it.
func literalOnly(expression ast.Expr) (only, hasString bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		return true, value.Kind == token.STRING
	case *ast.Ident:
		return value.Name == "nil" || value.Name == "true" || value.Name == "false", false
	case *ast.ParenExpr:
		return literalOnly(value.X)
	case *ast.UnaryExpr:
		return literalOnly(value.X)
	case *ast.BinaryExpr:
		leftOnly, leftString := literalOnly(value.X)
		rightOnly, rightString := literalOnly(value.Y)
		return leftOnly && rightOnly, leftString || rightString
	case *ast.KeyValueExpr:
		return literalOnly(value.Value)
	case *ast.CompositeLit:
		for _, element := range value.Elts {
			elementOnly, elementString := literalOnly(element)
			if !elementOnly {
				return false, false
			}
			hasString = hasString || elementString
		}
		return true, hasString
	default:
		return false, false
	}
}

// unconditionalLiteralVerdicts returns "name at line" for every function in
// the source that the tripwire describes.
func unconditionalLiteralVerdicts(t *testing.T, filename string, source []byte) []string {
	t.Helper()
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var found []string
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || !verdictFunctionNames[function.Name.Name] {
			continue
		}
		if len(function.Body.List) != 1 {
			continue
		}
		statement, ok := function.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(statement.Results) == 0 {
			continue
		}
		allLiteral, anyString := true, false
		for _, result := range statement.Results {
			only, hasString := literalOnly(result)
			allLiteral = allLiteral && only
			anyString = anyString || hasString
		}
		if allLiteral && anyString {
			position := files.Position(function.Pos())
			found = append(found, function.Name.Name+" at "+position.String())
		}
	}
	return found
}

func TestNoUnconditionalLiteralVerdicts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			checked++
			relative, _ := filepath.Rel(root, path)
			for _, finding := range unconditionalLiteralVerdicts(t, filepath.ToSlash(relative), source) {
				t.Errorf("%s returns a literal verdict unconditionally; a verdict must be computed from its input (rule R4)", finding)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 100 {
		t.Fatalf("only %d Go files were checked; the walk is not reading the repository", checked)
	}
}

func TestFabricationTripwireDetectsTheKnownShapes(t *testing.T) {
	flagged := map[string]string{
		"string verdict": `package p
func Review() string { return "ACCEPT" }`,
		"method with error": `package p
type r struct{}
func (r) Evaluate(input string) (string, error) { return "PASS", nil }`,
		"constant confidence": `package p
func Assess(x int) (string, float64) { return "ok", 0.95 }`,
		"struct of literals": `package p
type V struct{ Verdict, Reason string }
func Verdict() V { return V{Verdict: "ACCEPT", Reason: "looks good"} }`,
		"concatenated literals": `package p
func Review() string { return ("AC" + "CEPT") }`,
	}
	for name, source := range flagged {
		if found := unconditionalLiteralVerdicts(t, name+".go", []byte(source)); len(found) != 1 {
			t.Errorf("%s: findings = %v, want one", name, found)
		}
	}
	clean := map[string]string{
		"computed": `package p
func Review(ok bool) string { if ok { return "ACCEPT" }; return "REVISE" }`,
		"delegates": `package p
func judge(s string) string { return s }
func Evaluate(s string) string { return judge(s) }`,
		"uses input in a struct": `package p
type V struct{ Verdict, Reason string }
func Verdict(reason string) V { return V{Verdict: "BLOCK", Reason: reason} }`,
		"other name": `package p
func Name() string { return "reviewer" }`,
		"no string": `package p
func Assess() error { return nil }`,
		"declared without body is not flagged": `package p
func Review() string`,
	}
	for name, source := range clean {
		if found := unconditionalLiteralVerdicts(t, name+".go", []byte(source)); len(found) != 0 {
			t.Errorf("%s: findings = %v, want none", name, found)
		}
	}
}
