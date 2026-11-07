package daemon

// Baseline probe for issue #26: measures peak-heap signal of a rebuild over a
// large synthetic comment load, driving the existing rebuild entry point (no
// new API, so it compiles on origin/main and on the fixed branch alike).
//
// Load shape mirrors the production hot path: N issues x M comments, each
// comment a large body whose LAST line is one pm marker or plain text.
// rebuild -> read -> IssueComments -> markers.Parse holds every body until
// the markers are derived.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

const (
	probe26Issues   = 20
	probe26Comments = 200
	probe26PadBytes = 8 << 10 // 8 KiB of chatter per comment before the marker line
)

func probe26MarkerLine(t testing.TB, j int) (string, bool) {
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
		m = markers.Superseded{By: "o/pm#9"}
	}
	line, err := markers.Format(m)
	if err != nil {
		t.Fatalf("format %T: %v", m, err)
	}
	return line, true
}

func probe26Body(t testing.TB, i, j int) string {
	t.Helper()
	pad := strings.Repeat(fmt.Sprintf("chatter issue %d comment %d ", i, j), probe26PadBytes/32)
	line, ok := probe26MarkerLine(t, j)
	if !ok {
		return pad + "\nplain human discussion, no marker"
	}
	body := pad + "\n" + line + "\n"
	if _, err := markers.Parse(body); err != nil {
		t.Fatalf("probe fixture does not parse: %v", err)
	}
	return body
}

func probe26Daemon(t testing.TB, _ ...bool) (*Daemon, *fakeGitHub) {
	t.Helper()
	t1 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{
		items:    make([]github.Item, 0, probe26Issues),
		comments: map[int][]github.Comment{},
		blockers: map[int][]github.IssueRef{},
	}
	for i := range probe26Issues {
		n := i + 1
		gh.items = append(gh.items, github.Item{
			ID: fmt.Sprintf("i%d", n), Repo: "o/pm", Number: n,
			UpdatedAt: t1,
			Fields:    map[string]string{"Kind": "Agenda", "Agenda": fmt.Sprintf("D%d", n), "Status": "In Progress"},
		})
		cs := make([]github.Comment, 0, probe26Comments)
		for j := range probe26Comments {
			cs = append(cs, github.Comment{Body: probe26Body(t, n, j), CreatedAt: t1.Add(time.Duration(j) * time.Second)})
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

func snapshot26Issues(d *Daemon) []queue.Issue {
	out := make([]queue.Issue, 0, len(d.records))
	for _, r := range d.records {
		out = append(out, r.issue)
	}
	return out
}

func countMarkers(d *Daemon) int {
	n := 0
	for _, r := range d.records {
		n += len(r.issue.Markers)
	}
	return n
}

// BenchmarkRebuildLarge exercises the full rebuild over the large synthetic
// load and reports allocation totals. Run with -benchmem; compare before/after
// on the same machine. The fake holds the bodies (like the REST client must),
// so B/op measures parse + marker-retention work, not fixture storage.
func BenchmarkRebuildLarge(b *testing.B) {
	d, gh := probe26Daemon(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := range gh.items {
			gh.items[k].UpdatedAt = gh.items[k].UpdatedAt.Add(time.Hour)
		}
		if err := d.rebuild(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// PeakHeapMB samples heap in-use while rebuild runs and reports the maximum.
// Not a testing.B benchmark (allocation counters measure totals, not peaks);
// run with: go test ./internal/daemon -run TestProbePeakHeap -v
func TestProbePeakHeap(t *testing.T) {
	d, gh := probe26Daemon(t)
	// Force a full re-read on the next rebuild.
	for i := range gh.items {
		gh.items[i].UpdatedAt = gh.items[i].UpdatedAt.Add(time.Hour)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := d.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("PROBE issues=%d commentsPerIssue=%d padBytes=%d heapAllocDelta=%.2fMB totalAllocsDelta=%.2fMB numGC=%d totalBodies=%.2fMB liveMarkers=%d",
		probe26Issues, probe26Comments, probe26PadBytes,
		float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20),
		float64(after.TotalAlloc-before.TotalAlloc)/(1<<20),
		int64(after.NumGC)-int64(before.NumGC),
		float64(probe26Issues*probe26Comments*probe26PadBytes)/(1<<20),
		countMarkers(d))
}
