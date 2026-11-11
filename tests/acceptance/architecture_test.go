package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Guardrails of the development plan (rule R3): no package without a caller
// on a running path, no schema outside the migrations directory, migrations
// numbered without gaps. They read the repository the test runs in.

const modulePath = "github.com/SofiaFlux/summa42"

// orphanAllowlist names the internal packages that may exist without being a
// dependency of a command, each with the reason. Never extend it without a
// plan card that says so (development plan, card M0-T2).
var orphanAllowlist = map[string]string{
	"internal/testutil":              "test helper: real SQLite store for tests",
	"internal/securityregression":    "test-only package: security regression corpus",
	"internal/ghtriage/fakemodel":    "test helper: scripted triage model",
	"internal/modelrun/modelruntest": "test helper: launcher test double",
	"internal/board":                 "until card M2-T6 wires it; M2-T6 removes this entry",
	"internal/version":               "until card M1-T2 prints it; M1-T2 removes this entry",
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func goList(t *testing.T, root string, arguments ...string) []string {
	t.Helper()
	command := exec.Command("go", append([]string{"list"}, arguments...)...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		detail := ""
		if exit, ok := err.(*exec.ExitError); ok {
			detail = string(exit.Stderr)
		}
		t.Fatalf("go list %s: %v\n%s", strings.Join(arguments, " "), err, detail)
	}
	return strings.Fields(string(output))
}

// orphanPackages returns the internal packages that no command depends on and
// the allowlist does not excuse, and the allowlist entries that are stale
// (the package has a caller now, or no longer exists).
func orphanPackages(internal, commandDeps []string, allow map[string]string) (orphans, stale []string) {
	reached := make(map[string]bool, len(commandDeps))
	for _, pkg := range commandDeps {
		reached[pkg] = true
	}
	present := make(map[string]bool, len(internal))
	for _, pkg := range internal {
		relative := strings.TrimPrefix(pkg, modulePath+"/")
		present[relative] = true
		if reached[pkg] {
			continue
		}
		if _, excused := allow[relative]; !excused {
			orphans = append(orphans, relative)
		}
	}
	for relative := range allow {
		if !present[relative] || reached[modulePath+"/"+relative] {
			stale = append(stale, relative)
		}
	}
	sort.Strings(orphans)
	sort.Strings(stale)
	return orphans, stale
}

func TestEveryInternalPackageHasACaller(t *testing.T) {
	root := repositoryRoot(t)
	internal := goList(t, root, "./internal/...")
	deps := goList(t, root, "-deps", "./cmd/...")
	if len(internal) == 0 || len(deps) == 0 {
		t.Fatalf("go list returned nothing: %d internal packages, %d dependencies", len(internal), len(deps))
	}
	orphans, stale := orphanPackages(internal, deps, orphanAllowlist)
	for _, pkg := range orphans {
		t.Errorf("%s is not a dependency of any command under cmd/: add a caller on a running path in the same change (rule R3), or park the package", pkg)
	}
	for _, pkg := range stale {
		t.Errorf("%s is on the orphan allowlist but has a caller now or no longer exists: remove the entry", pkg)
	}
	for pkg, reason := range orphanAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("orphan allowlist entry %s has no written reason", pkg)
		}
	}
}

func TestOrphanGateFailsForANewPackageWithoutACaller(t *testing.T) {
	internal := []string{
		modulePath + "/internal/used",
		modulePath + "/internal/brandnew",
		modulePath + "/internal/helper",
		modulePath + "/internal/wired",
	}
	deps := []string{"fmt", modulePath + "/internal/used", modulePath + "/internal/wired"}
	allow := map[string]string{
		"internal/helper": "test helper",
		"internal/wired":  "was parked, has a caller now",
		"internal/gone":   "deleted package",
	}
	orphans, stale := orphanPackages(internal, deps, allow)
	if len(orphans) != 1 || orphans[0] != "internal/brandnew" {
		t.Fatalf("orphans = %v, want only internal/brandnew", orphans)
	}
	if len(stale) != 2 || stale[0] != "internal/gone" || stale[1] != "internal/wired" {
		t.Fatalf("stale = %v, want internal/gone and internal/wired", stale)
	}
}

var schemaStatement = regexp.MustCompile(`(?i)\b(CREATE\s+(UNIQUE\s+)?(TABLE|INDEX)|ALTER\s+TABLE)\b`)

const migrationsDirectory = "internal/state/sqlite/migrations"

func TestNoSchemaOutsideMigrations(t *testing.T) {
	root := repositoryRoot(t)
	checked := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == ".git" || relative == migrationsDirectory {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(relative, ".go") || strings.HasSuffix(relative, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		for number, line := range strings.Split(string(content), "\n") {
			if schemaStatement.MatchString(line) {
				t.Errorf("%s:%d: schema statement outside %s; add a numbered migration instead (rule R3)", relative, number+1, migrationsDirectory)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Go files were checked")
	}
}

func TestSchemaStatementPattern(t *testing.T) {
	for _, text := range []string{
		"CREATE TABLE x (id TEXT)",
		"create table if not exists x (id TEXT)",
		"ALTER  TABLE x ADD COLUMN y",
		"CREATE INDEX x_y ON x(y)",
		"CREATE UNIQUE INDEX x_y ON x(y)",
		"db.Exec(`CREATE\tTABLE x`)",
	} {
		if !schemaStatement.MatchString(text) {
			t.Errorf("not matched: %q", text)
		}
	}
	for _, text := range []string{
		"SELECT name FROM sqlite_master WHERE type = 'table'",
		"// recreate tablets",
		"INSERT INTO created_index VALUES (1)",
	} {
		if schemaStatement.MatchString(text) {
			t.Errorf("matched: %q", text)
		}
	}
}

var migrationName = regexp.MustCompile(`^(\d{5})_[a-z0-9_]+\.sql$`)

// migrationProblems reports names that do not follow NNNNN_name.sql and
// numbers that repeat or leave a gap; numbering starts at 1.
func migrationProblems(names []string) []string {
	var problems []string
	seen := map[int]string{}
	highest := 0
	for _, name := range names {
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			problems = append(problems, name+": name is not NNNNN_lower_snake.sql")
			continue
		}
		number, _ := strconv.Atoi(match[1])
		if number == 0 {
			problems = append(problems, name+": numbering starts at 00001")
			continue
		}
		if other, duplicate := seen[number]; duplicate {
			problems = append(problems, name+": same number as "+other)
			continue
		}
		seen[number] = name
		if number > highest {
			highest = number
		}
	}
	for number := 1; number <= highest; number++ {
		if _, ok := seen[number]; !ok {
			problems = append(problems, "missing migration number "+strconv.Itoa(number))
		}
	}
	return problems
}

func TestMigrationsAreSequential(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repositoryRoot(t), filepath.FromSlash(migrationsDirectory)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		t.Fatal("no migrations found")
	}
	for _, problem := range migrationProblems(names) {
		t.Error(problem)
	}
}

func TestMigrationGateDetectsGapsAndDuplicates(t *testing.T) {
	if problems := migrationProblems([]string{"00001_a.sql", "00002_b.sql", "00003_c.sql"}); len(problems) != 0 {
		t.Fatalf("clean sequence reported: %v", problems)
	}
	problems := strings.Join(migrationProblems([]string{
		"00001_a.sql", "00003_c.sql", "00003_d.sql", "0005_short.sql", "00000_zero.sql",
	}), "\n")
	for _, want := range []string{
		"missing migration number 2",
		"00003_d.sql: same number as 00003_c.sql",
		"0005_short.sql: name is not",
		"00000_zero.sql: numbering starts",
	} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems do not mention %q:\n%s", want, problems)
		}
	}
}
