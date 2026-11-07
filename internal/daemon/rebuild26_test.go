package daemon

// Rebuild streaming carries exactly the derived state: forcing a full re-read of
// the same fixture must reproduce every record (markers, PanelAt, BlockedBy)
// and the derived queue, marker for marker.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-council/internal/queue"
)

func TestRebuildStreamingReproducesRecords(t *testing.T) {
	d, gh := probe26Daemon(t)
	before := map[int]record{}
	for n, r := range d.records {
		before[n] = r
	}
	qBefore := queue.Build(queue.Snapshot{Issues: snapshot26Issues(d), Seats: []queue.Seat{{ID: "A"}}})
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
	qAfter := queue.Build(queue.Snapshot{Issues: snapshot26Issues(d), Seats: []queue.Seat{{ID: "A"}}})
	if !reflect.DeepEqual(qAfter, qBefore) {
		t.Fatalf("queue differs after reread:\n got %+v\nwant %+v", qAfter, qBefore)
	}
}
