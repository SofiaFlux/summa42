package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishCLIRejectsIncompleteAndUnsafeConfiguration(t *testing.T) {
	if runGHPublish(nil, nil) == nil {
		t.Fatal("nil context")
	}
	for _, args := range [][]string{nil, {"--case", "c", "--source-repo", "relative", "--base-branch", "main", "--repository", "o/r", "--credential-file", "/missing"}, {"--case", "c", "--source-repo", "/tmp", "--base-branch", "main", "--repository", "o/r", "--credential-file", "relative"}} {
		if runGHPublish(context.Background(), args) == nil {
			t.Fatal("accepted incomplete config")
		}
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("private-test-token"), 0644)
	if runGHPublish(context.Background(), []string{"--case", "c", "--source-repo", dir, "--base-branch", "main", "--repository", "o/r", "--credential-file", token}) == nil {
		t.Fatal("public token")
	}
}
