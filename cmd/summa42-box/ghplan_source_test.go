package main

import (
	"flag"
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
