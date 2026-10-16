package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeCLIRejectsIncompleteAndUnsafeConfiguration(t *testing.T) {
	if runGHMerge(nil, nil) == nil {
		t.Fatal("nil context accepted")
	}
	for _, args := range [][]string{
		nil,
		{"--case", "c", "--source-repo", "relative", "--base-branch", "main", "--repository", "o/r", "--credential-file", "/missing"},
		{"--case", "c", "--source-repo", "/tmp", "--base-branch", "main", "--repository", "o/r", "--credential-file", "relative"},
		{"--case", "", "--source-repo", "/tmp", "--base-branch", "main", "--repository", "o/r", "--credential-file", "/tmp/token"},
	} {
		if runGHMerge(context.Background(), args) == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("private-token"), 0644)
	if runGHMerge(context.Background(), []string{"--case", "c", "--source-repo", dir, "--base-branch", "main", "--repository", "o/r", "--credential-file", token}) == nil {
		t.Fatal("accepted public token file without 0600 permissions")
	}
}
