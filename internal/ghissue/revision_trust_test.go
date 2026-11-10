package ghissue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// collectOne lists one issue through a real Client configured with maintainers.
func collectOne(t *testing.T, updated, comments string, extraIssueFields string) Issue {
	t.Helper()
	issue := `{"number":7,"title":"t","body":"","state":"open","user":{"login":"maint"},"assignees":[],"labels":[],"comments":2,` +
		extraIssueFields + `"created_at":"2026-09-24T08:00:00Z","updated_at":"` + updated + `","html_url":"https://github.com/o/r/issues/7"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = w.Write([]byte(comments))
			return
		}
		_, _ = w.Write([]byte("[" + issue + "]"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Repository: "o/r", TokenFile: tokenFile(t, "tok\n"), Maintainers: []string{"Maint"}})
	if err != nil {
		t.Fatal(err)
	}
	issues, bad, err := CollectIssues(context.Background(), client)
	if err != nil || len(bad) != 0 || len(issues) != 1 {
		t.Fatalf("collect: %v bad=%d issues=%d", err, len(bad), len(issues))
	}
	return issues[0]
}

func TestCommentByNonMaintainerIsAnUntrustedTail(t *testing.T) {
	// A maintainer commented at 09:00, then someone else at 10:00.
	issue := collectOne(t, "2026-09-24T10:00:00Z",
		`[{"user":{"login":"maint"},"updated_at":"2026-09-24T09:00:00Z"},{"user":{"login":"stranger"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if !issue.UntrustedTail || !issue.TrustedFloor || issue.RevisionID() != "2026-09-24T09:00:00Z" {
		t.Fatalf("untrusted=%v floor=%v revision=%s; want an untrusted tail on the maintainer's 09:00 comment",
			issue.UntrustedTail, issue.TrustedFloor, issue.RevisionID())
	}

	// Only a non-maintainer commented: no trusted comment to fall back to.
	issue = collectOne(t, "2026-09-24T10:00:00Z", `[{"user":{"login":"stranger"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if !issue.UntrustedTail || issue.TrustedFloor || issue.RevisionID() != "2026-09-24T10:00:00Z" {
		t.Fatalf("untrusted=%v floor=%v revision=%s; want an untrusted tail without a floor",
			issue.UntrustedTail, issue.TrustedFloor, issue.RevisionID())
	}

	// The maintainer's comment is the latest update: an ordinary revision.
	issue = collectOne(t, "2026-09-24T10:00:00Z",
		`[{"user":{"login":"stranger"},"updated_at":"2026-09-24T09:00:00Z"},{"user":{"login":"MAINT"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if issue.UntrustedTail || issue.RevisionID() != "2026-09-24T10:00:00Z" {
		t.Fatalf("untrusted=%v revision=%s; a maintainer's comment must move the revision", issue.UntrustedTail, issue.RevisionID())
	}

	// The issue changed after the stranger's comment (an edit or a label): trusted again.
	issue = collectOne(t, "2026-09-24T11:00:00Z", `[{"user":{"login":"stranger"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if issue.UntrustedTail || issue.RevisionID() != "2026-09-24T11:00:00Z" {
		t.Fatalf("untrusted=%v revision=%s; an update after the comment is a new revision", issue.UntrustedTail, issue.RevisionID())
	}
}

func TestTrustMarkersInTheResponseAreIgnored(t *testing.T) {
	issue := collectOne(t, "2026-09-24T10:00:00Z",
		`[{"user":{"login":"maint"},"updated_at":"2026-09-24T10:00:00Z"}]`,
		`"summa42_untrusted_tail":true,"summa42_trusted_floor":true,`)
	if issue.UntrustedTail || issue.TrustedFloor {
		t.Fatalf("untrusted=%v floor=%v; markers supplied by the server must not be believed", issue.UntrustedTail, issue.TrustedFloor)
	}
}

func observedRevisions(t *testing.T, f observeFixture) []string {
	t.Helper()
	cases, err := f.cases.ListByObject(f.ctx, f.cfg.MissionID, "github", "github:o/r#7")
	if err != nil {
		t.Fatal(err)
	}
	revisions := make([]string, 0, len(cases))
	for _, c := range cases {
		revisions = append(revisions, c.RevisionID)
	}
	return revisions
}

func TestObserveKeepsRecordedRevisionAcrossUntrustedComments(t *testing.T) {
	f := setupObserve(t)
	observe := func(updated string, untrustedTail, trustedFloor bool) {
		t.Helper()
		lister := &stubLister{name: "o/r", pages: []stubPage{{items: []any{wireIssue(func(m map[string]any) {
			m["updated_at"] = updated
			if untrustedTail {
				m[wireUntrustedTail] = true
			}
			if trustedFloor {
				m[wireTrustedFloor] = true
			}
		})}}}}
		if _, err := ObserveOnce(f.ctx, lister, f.cases, f.execSvc, f.evidenceStore, f.cfg); err != nil {
			t.Fatal(err)
		}
	}
	wantRevisions := func(when string, want ...string) {
		t.Helper()
		got := observedRevisions(t, f)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: recorded revisions = %v, want %v", when, got, want)
		}
	}

	// The maintainer's issue is recorded at its 11:00 edit.
	observe("2026-09-24T11:00:00Z", false, false)
	wantRevisions("first sight", "2026-09-24T11:00:00Z")

	// A stranger comments at 12:00; no maintainer comment exists.
	observe("2026-09-24T12:00:00Z", true, false)
	wantRevisions("stranger comment without a floor", "2026-09-24T11:00:00Z")

	// A stranger comments while the last maintainer comment (10:00) is older
	// than the recorded 11:00 edit: the revision must not go back to 10:00.
	observe("2026-09-24T10:00:00Z", true, true)
	wantRevisions("stranger comment with an older floor", "2026-09-24T11:00:00Z")

	// A maintainer comment at 13:00 that was not observed before a stranger's
	// 14:00 comment still moves the revision.
	observe("2026-09-24T13:00:00Z", true, true)
	wantRevisions("unobserved maintainer comment under a stranger's", "2026-09-24T11:00:00Z", "2026-09-24T13:00:00Z")

	// And a maintainer's own later update moves it as before.
	observe("2026-09-24T15:00:00Z", false, false)
	wantRevisions("maintainer update", "2026-09-24T11:00:00Z", "2026-09-24T13:00:00Z", "2026-09-24T15:00:00Z")
}

func TestObserveFirstSightUnderAnUntrustedCommentIsRecordedOnce(t *testing.T) {
	f := setupObserve(t)
	for _, updated := range []string{"2026-09-24T12:00:00Z", "2026-09-24T12:30:00Z"} {
		lister := &stubLister{name: "o/r", pages: []stubPage{{items: []any{wireIssue(func(m map[string]any) {
			m["updated_at"] = updated
			m[wireUntrustedTail] = true
		})}}}}
		if _, err := ObserveOnce(f.ctx, lister, f.cases, f.execSvc, f.evidenceStore, f.cfg); err != nil {
			t.Fatal(err)
		}
	}
	if got := observedRevisions(t, f); len(got) != 1 || got[0] != "2026-09-24T12:00:00Z" {
		t.Fatalf("recorded revisions = %v, want only the first sight at 12:00", got)
	}
}
