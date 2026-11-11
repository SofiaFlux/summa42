package ghissue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
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

func TestClientReportsWhoMovedTheIssueLast(t *testing.T) {
	at := func(clock string) time.Time {
		parsed, err := time.Parse(time.RFC3339, "2026-09-24T"+clock+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	// A maintainer commented at 09:00, then someone else at 10:00.
	issue := collectOne(t, "2026-09-24T10:00:00Z",
		`[{"user":{"login":"maint"},"updated_at":"2026-09-24T09:00:00Z"},{"user":{"login":"stranger"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if !issue.UntrustedTail || !issue.LatestTrustedComment.Equal(at("09:00:00")) || !issue.RawUpdatedAt.Equal(at("10:00:00")) || issue.RevisionID() != "2026-09-24T09:00:00Z" {
		t.Fatalf("untrusted=%v trusted=%s raw=%s revision=%s; want an untrusted tail resting on the maintainer's 09:00 comment",
			issue.UntrustedTail, issue.LatestTrustedComment, issue.RawUpdatedAt, issue.RevisionID())
	}

	// Only a non-maintainer commented: nothing trusted to rest on.
	issue = collectOne(t, "2026-09-24T10:00:00Z", `[{"user":{"login":"stranger"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if !issue.UntrustedTail || !issue.LatestTrustedComment.IsZero() || !issue.RawUpdatedAt.IsZero() || issue.RevisionID() != "2026-09-24T10:00:00Z" {
		t.Fatalf("untrusted=%v trusted=%s raw=%s revision=%s; want an untrusted tail with plain updated_at",
			issue.UntrustedTail, issue.LatestTrustedComment, issue.RawUpdatedAt, issue.RevisionID())
	}

	// The maintainer's comment is the latest update.
	issue = collectOne(t, "2026-09-24T10:00:00Z",
		`[{"user":{"login":"stranger"},"updated_at":"2026-09-24T09:00:00Z"},{"user":{"login":"MAINT"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if issue.UntrustedTail || !issue.LatestTrustedComment.Equal(at("10:00:00")) || issue.RevisionID() != "2026-09-24T10:00:00Z" {
		t.Fatalf("untrusted=%v trusted=%s revision=%s; want the maintainer's 10:00 comment reported",
			issue.UntrustedTail, issue.LatestTrustedComment, issue.RevisionID())
	}

	// The issue changed after the stranger's comment: not a comment tail.
	issue = collectOne(t, "2026-09-24T11:00:00Z", `[{"user":{"login":"stranger"},"updated_at":"2026-09-24T10:00:00Z"}]`, "")
	if issue.UntrustedTail || issue.RevisionID() != "2026-09-24T11:00:00Z" {
		t.Fatalf("untrusted=%v revision=%s; an update after the comment is not an untrusted tail", issue.UntrustedTail, issue.RevisionID())
	}
}

func TestTrustMarkersInTheResponseAreIgnored(t *testing.T) {
	issue := collectOne(t, "2026-09-24T10:00:00Z",
		`[{"user":{"login":"stranger"},"updated_at":"2026-09-24T09:00:00Z"}]`,
		`"summa42_untrusted_tail":true,"summa42_latest_trusted_comment":"2026-09-24T09:30:00Z","summa42_raw_updated_at":"2026-09-24T08:00:00Z",`)
	if issue.UntrustedTail || !issue.LatestTrustedComment.IsZero() || !issue.RawUpdatedAt.IsZero() {
		t.Fatalf("untrusted=%v trusted=%s raw=%s; markers supplied by the server must not be believed",
			issue.UntrustedTail, issue.LatestTrustedComment, issue.RawUpdatedAt)
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
	sort.Strings(revisions)
	return revisions
}

// observation is one poll result for issue #7 as the Client would report it.
type observation struct {
	updated, raw, trusted string // clock times on 2026-09-24; empty = not reported
	untrustedTail         bool
	body                  string
}

func observe(t *testing.T, f observeFixture, o observation) {
	t.Helper()
	stamp := func(clock string) string { return "2026-09-24T" + clock + "Z" }
	lister := &stubLister{name: "o/r", pages: []stubPage{{items: []any{wireIssue(func(m map[string]any) {
		m["updated_at"] = stamp(o.updated)
		if o.body != "" {
			m["body"] = o.body
		}
		if o.untrustedTail {
			m[wireUntrustedTail] = true
		}
		if o.raw != "" {
			m[wireRawUpdatedAt] = stamp(o.raw)
		}
		if o.trusted != "" {
			m[wireLatestTrustedComment] = stamp(o.trusted)
		}
	})}}}}
	if _, err := ObserveOnce(f.ctx, lister, f.cases, f.execSvc, f.evidenceStore, f.cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionMovesOnlyOnMaintainerCommentOrContentChange(t *testing.T) {
	f := setupObserve(t)
	want := func(when string, clocks ...string) {
		t.Helper()
		expected := make([]string, 0, len(clocks))
		for _, clock := range clocks {
			expected = append(expected, "2026-09-24T"+clock+"Z")
		}
		if got := observedRevisions(t, f); strings.Join(got, ",") != strings.Join(expected, ",") {
			t.Fatalf("%s: recorded revisions = %v, want %v", when, got, expected)
		}
	}

	observe(t, f, observation{updated: "11:00:00"})
	want("first sight", "11:00:00")

	// updated_at moved with no comment behind it: a reference from another
	// issue, or a comment that was posted and deleted.
	observe(t, f, observation{updated: "12:00:00"})
	want("updated_at moved, nothing changed", "11:00:00")

	// A stranger's comment is the latest update; no maintainer ever commented.
	observe(t, f, observation{updated: "12:30:00", untrustedTail: true})
	want("stranger comment", "11:00:00")

	// A stranger's comment over a maintainer comment older than the recorded
	// revision: the Client reports 10:00, the revision must not go back to it.
	observe(t, f, observation{updated: "10:00:00", raw: "13:00:00", trusted: "10:00:00", untrustedTail: true})
	want("stranger comment over an older maintainer comment", "11:00:00")

	// A maintainer comments: the content is the same, the revision moves.
	observe(t, f, observation{updated: "14:00:00", trusted: "14:00:00"})
	want("maintainer comment", "11:00:00", "14:00:00")

	// A maintainer comment that a stranger's later comment hides from updated_at.
	observe(t, f, observation{updated: "15:00:00", raw: "15:30:00", trusted: "15:00:00", untrustedTail: true})
	want("maintainer comment under a stranger's", "11:00:00", "14:00:00", "15:00:00")

	// The maintainer edits the body.
	observe(t, f, observation{updated: "16:00:00", trusted: "15:00:00", body: "steps, revised"})
	want("body edit", "11:00:00", "14:00:00", "15:00:00", "16:00:00")

	// Another edit, followed by a stranger's comment before the next poll: the
	// Client rests on the 15:00 comment, but the content did change.
	observe(t, f, observation{updated: "15:00:00", raw: "17:00:00", trusted: "15:00:00", untrustedTail: true, body: "steps, revised twice"})
	want("body edit under a stranger's comment", "11:00:00", "14:00:00", "15:00:00", "16:00:00", "17:00:00")

	// Polling the same state again changes nothing.
	observe(t, f, observation{updated: "15:00:00", raw: "17:00:00", trusted: "15:00:00", untrustedTail: true, body: "steps, revised twice"})
	observe(t, f, observation{updated: "18:00:00", trusted: "15:00:00", body: "steps, revised twice"})
	want("repoll and another unrelated update", "11:00:00", "14:00:00", "15:00:00", "16:00:00", "17:00:00")
}

func TestFirstSightUnderAStrangersCommentIsRecordedOnce(t *testing.T) {
	f := setupObserve(t)
	observe(t, f, observation{updated: "12:00:00", untrustedTail: true})
	observe(t, f, observation{updated: "12:30:00", untrustedTail: true})
	if got := observedRevisions(t, f); len(got) != 1 || got[0] != "2026-09-24T12:00:00Z" {
		t.Fatalf("recorded revisions = %v, want only the first sight at 12:00", got)
	}
}
