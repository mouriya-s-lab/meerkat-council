package daemon

// Rebuild streaming carries exactly the derived state: forcing a full re-read of
// the same fixture must reproduce every record (markers, PanelAt, BlockedBy)
// and the derived queue, marker for marker.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mouriya-s-lab/meerkat-council/internal/github"
	"github.com/mouriya-s-lab/meerkat-council/internal/markers"
	"github.com/mouriya-s-lab/meerkat-council/internal/queue"
)

const (
	rebuildIssues   = 20
	rebuildComments = 200
	rebuildPadBytes = 8 << 10 // 8 KiB of chatter per comment before the marker line
)

func markerLine(t *testing.T, j int) (string, bool) {
	t.Helper()
	var m markers.Marker
	switch j % 4 {
	case 0:
		m = markers.Panel{Event: markers.PanelStarted, Round: 1, Sessions: []markers.PanelSession{}}
	case 1:
		m = markers.Vote{Seat: "A", Version: 1, Rows: []markers.VoteRow{{Row: "D1-01", Choice: markers.ChoiceApprove}}}
	case 2:
		return "", false
	default:
		m = markers.Superseded{By: "o/agenda#9"}
	}
	line, err := markers.Format(m)
	if err != nil {
		t.Fatalf("format %T: %v", m, err)
	}
	return line, true
}

func commentBody(t *testing.T, i, j int) string {
	t.Helper()
	pad := strings.Repeat(fmt.Sprintf("chatter issue %d comment %d ", i, j), rebuildPadBytes/32)
	line, ok := markerLine(t, j)
	if !ok {
		return pad + "\nplain human discussion, no marker"
	}
	body := pad + "\n" + line + "\n"
	if _, err := markers.Parse(body); err != nil {
		t.Fatalf("rebuild fixture does not parse: %v", err)
	}
	return body
}

func rebuildDaemon(t *testing.T) (*Daemon, *fakeGitHub) {
	t.Helper()
	t1 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		items:    make([]github.Item, 0, rebuildIssues),
		comments: map[int][]github.Comment{},
		blockers: map[int][]github.IssueRef{},
	}
	for i := range rebuildIssues {
		n := i + 1
		gh.items = append(gh.items, github.Item{
			ID: fmt.Sprintf("i%d", n), Repo: "o/agenda", Number: n,
			UpdatedAt: t1,
			Fields:    map[string]string{"Kind": "Agenda", "Agenda": fmt.Sprintf("D%d", n), "Status": "In Progress"},
		})
		cs := make([]github.Comment, 0, rebuildComments)
		for j := range rebuildComments {
			cs = append(cs, github.Comment{Body: commentBody(t, n, j), CreatedAt: t1.Add(time.Duration(j) * time.Second)})
		}
		gh.comments[n] = cs
	}
	d, err := New(context.Background(), testConfig(), gh, project, &fakePanels{})
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: every issue parsed at least one marker, so the load exercises the marker path.
	for n, r := range d.records {
		if len(r.issue.Markers) == 0 {
			t.Fatalf("issue %d parsed zero markers", n)
		}
	}
	return d, gh
}

func snapshotIssues(d *Daemon) []queue.Issue {
	out := make([]queue.Issue, 0, len(d.records))
	for _, r := range d.records {
		out = append(out, r.issue)
	}
	return out
}

func TestRebuildStreamingReproducesRecords(t *testing.T) {
	d, gh := rebuildDaemon(t)
	before := map[int]record{}
	for n, r := range d.records {
		before[n] = r
	}
	qBefore := queue.Build(queue.Snapshot{Issues: snapshotIssues(d), Seats: []queue.Seat{{ID: "A"}}})
	for i := range gh.items {
		gh.items[i].UpdatedAt = gh.items[i].UpdatedAt.Add(time.Hour)
	}
	if err := d.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.records) != len(before) {
		t.Fatalf("records %d, want %d", len(d.records), len(before))
	}
	for n, r := range d.records {
		b := before[n]
		if !reflect.DeepEqual(r.issue.Markers, b.issue.Markers) {
			t.Fatalf("issue %d markers differ:\n got %#v\nwant %#v", n, r.issue.Markers, b.issue.Markers)
		}
		if !r.issue.PanelAt.Equal(b.issue.PanelAt) {
			t.Fatalf("issue %d PanelAt %v, want %v", n, r.issue.PanelAt, b.issue.PanelAt)
		}
		if !reflect.DeepEqual(r.issue.BlockedBy, b.issue.BlockedBy) {
			t.Fatalf("issue %d BlockedBy %q, want %q", n, r.issue.BlockedBy, b.issue.BlockedBy)
		}
	}
	qAfter := queue.Build(queue.Snapshot{Issues: snapshotIssues(d), Seats: []queue.Seat{{ID: "A"}}})
	if !reflect.DeepEqual(qAfter, qBefore) {
		t.Fatalf("queue differs after reread:\n got %+v\nwant %+v", qAfter, qBefore)
	}
}
