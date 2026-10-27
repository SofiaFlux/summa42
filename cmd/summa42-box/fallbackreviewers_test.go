package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
)

var (
	testReviewer = climodel.Config{Binary: "/opt/cmd", RunAsUser: "summa42-model", Timeout: time.Minute,
		CommandCodeModel: "deepseek/deepseek-v4-pro", AllowPaid: true}
	testProducer = climodel.Config{Binary: "/opt/agy", RunAsUser: "summa42-model", Timeout: time.Minute,
		Profile: "agy", ProfileModel: "gemini-3.8-flash-high"}
)

func writeFallbackList(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fallback-reviewers.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFallbackListIsOptional(t *testing.T) {
	got, err := loadFallbackReviewers(filepath.Join(t.TempDir(), "absent.json"), testReviewer, testProducer)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing list = %v, %v; want empty and no error", got, err)
	}
}

func TestFallbackAcceptsAFreeCommandCodeModelWithTheReviewersSettings(t *testing.T) {
	path := writeFallbackList(t, `{"reviewers":[{"harness":"commandcode","model":"reflection/beam:free","accepts_no_zdr":true}]}`, 0o600)
	got, err := loadFallbackReviewers(path, testReviewer, testProducer)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CommandCodeModel != "reflection/beam:free" || got[0].AllowPaid || got[0].Binary != testReviewer.Binary {
		t.Fatalf("fallback config = %+v", got)
	}
}

func TestFallbackRefusesWhatTheOwnerDidNotApprove(t *testing.T) {
	cases := map[string]string{
		"claude is out for now":   `{"reviewers":[{"harness":"claude","model":"claude-opus-5-5","accepts_no_zdr":true}]}`,
		"paid command code":       `{"reviewers":[{"harness":"commandcode","model":"deepseek/deepseek-v4-pro","accepts_no_zdr":true}]}`,
		"no zdr not accepted":     `{"reviewers":[{"harness":"commandcode","model":"reflection/beam:free"}]}`,
		"the producer itself":     `{"reviewers":[{"harness":"agy","model":"gemini-3.8-flash-high","accepts_no_zdr":true}]}`,
		"unknown field":           `{"reviewers":[{"harness":"commandcode","model":"reflection/beam:free","accepts_no_zdr":true,"budget":9}]}`,
		"opencode without a path": `{"reviewers":[{"harness":"opencode","model":"opencode/big-pickle","accepts_no_zdr":true}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFallbackList(t, body, 0o600)
			if _, err := loadFallbackReviewers(path, testReviewer, testProducer); err == nil {
				t.Fatalf("list %s was accepted", body)
			}
		})
	}
}

func TestFallbackFileWritableByOthersIsRefused(t *testing.T) {
	path := writeFallbackList(t, `{"reviewers":[]}`, 0o666)
	if _, err := loadFallbackReviewers(path, testReviewer, testProducer); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Fatalf("writable list accepted: %v", err)
	}
}

func TestSameModelMatchesTheReviewerRule(t *testing.T) {
	if !sameModel(testProducer, testProducer) {
		t.Fatal("a config is not the same model as itself")
	}
	if sameModel(testProducer, testReviewer) {
		t.Fatal("producer and reviewer were reported as the same model")
	}
}
