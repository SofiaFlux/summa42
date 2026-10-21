package main

import (
	"flag"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"testing"
)

func TestGroundingFlagsRequireCompleteContract(t *testing.T) {
	for _, args := range [][]string{{"--source-repo", "/tmp/repo"}, {"--allowed-path", "README.md"}, {"--source-sha", "HEAD"}} {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		source := registerGroundingFlags(flags)
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		if _, err := source.config(); err == nil {
			t.Fatalf("partial source accepted: %v", args)
		}
	}
}

func TestExplicitEmptyGroundingFlagsCannotSelectLegacyPlan(t *testing.T) {
	for _, arg := range []string{"--source-repo=", "--source-sha=", "--source-repository="} {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		source := registerGroundingFlags(flags)
		if err := flags.Parse([]string{arg}); err != nil {
			t.Fatal(err)
		}
		if _, err := source.config(); err == nil {
			t.Fatalf("explicit empty source selected v1: %s", arg)
		}
	}
}
func TestGroundingFlagsPreserveLiteralCommandArgv(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	source := registerGroundingFlags(flags)
	if err := flags.Parse([]string{"--source-repo", "/tmp/repo", "--source-repository", "o/r", "--source-sha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--context-file", "README.md", "--allowed-path", "README.md", "--validation-command-json", `["go","test","./..."]`}); err != nil {
		t.Fatal(err)
	}
	cfg, err := source.config()
	if err != nil || cfg == nil || cfg.ValidationCommands[0][2] != "./..." {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestAllowedPrefixAndPathMutuallyExclusive(t *testing.T) {
	base := []string{"--source-repo", "/tmp/repo", "--source-repository", "o/r", "--source-sha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--validation-command-json", `["go","test","./..."]`}
	parse := func(extra ...string) (*ghtriage.GroundingConfig, error) {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		source := registerGroundingFlags(flags)
		if err := flags.Parse(append(append([]string(nil), base...), extra...)); err != nil {
			t.Fatal(err)
		}
		return source.config()
	}
	if _, err := parse("--allowed-prefix", "internal/", "--allowed-path", "README.md", "--context-file", "README.md"); err == nil {
		t.Fatal("prefix and exact path accepted together")
	}
	for _, bad := range []string{"internal", "/internal/", "../x/", "internal//"} {
		if _, err := parse("--allowed-prefix", bad); err == nil {
			t.Fatalf("unsafe prefix %q accepted", bad)
		}
	}
	cfg, err := parse("--allowed-prefix", "internal/")
	if err != nil || cfg == nil || cfg.AllowedPrefixes[0] != "internal/" || len(cfg.AllowedPaths) != 0 {
		t.Fatalf("prefix mode without context file: cfg=%+v err=%v", cfg, err)
	}
	if _, err := parse("--allowed-path", "README.md"); err == nil {
		t.Fatal("exact mode accepted without --context-file")
	}
}
