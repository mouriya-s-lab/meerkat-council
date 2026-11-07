package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/panel"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// fakePanels reports each started job and holds it until its context ends, then reports the cause.
type fakePanels struct {
	started chan panel.Job
	ended   chan error
}

func (f *fakePanels) Run(ctx context.Context, j panel.Job) {
	f.started <- j
	<-ctx.Done()
	f.ended <- context.Cause(ctx)
}

type write struct{ item, field, value string }

type fakeGitHub struct {
	items    []github.Item
	comments map[int][]github.Comment
	blockers map[int][]github.IssueRef
	writes   []write
	// call counts of the REST reads a rebuild may reuse its way past.
	commentsCalls map[int]int
	blockedCalls  map[int]int
}

func (f *fakeGitHub) ProjectItems(context.Context, string) ([]github.Item, error) {
	return f.items, nil
}

func (f *fakeGitHub) IssueItem(_ context.Context, repo string, n int, _ string) (github.Item, bool, error) {
	for _, it := range f.items {
		if it.Repo == repo && it.Number == n {
			return it, true, nil
		}
	}
	return github.Item{}, false, nil
}

func (f *fakeGitHub) IssueComments(_ context.Context, _ string, n int) ([]github.Comment, error) {
	if f.commentsCalls != nil {
		f.commentsCalls[n]++
	}
	return f.comments[n], nil
}

func (f *fakeGitHub) EachComment(_ context.Context, _ string, n int, fn func(string, time.Time) error) error {
	if f.commentsCalls != nil {
		f.commentsCalls[n]++
	}
	for _, c := range f.comments[n] {
		if err := fn(c.Body, c.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGitHub) BlockedBy(_ context.Context, _ string, n int) ([]github.IssueRef, error) {
	if f.blockedCalls != nil {
		f.blockedCalls[n]++
	}
	return f.blockers[n], nil
}

func (f *fakeGitHub) SetText(_ context.Context, _, item, field, text string) error {
	f.writes = append(f.writes, write{item, field, text})
	return nil
}

func (f *fakeGitHub) SetOption(_ context.Context, _, item, field, option string) error {
	f.writes = append(f.writes, write{item, field, option})
	return nil
}

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Repos.PM.Repo = "o/pm"
	cfg.Council.Agenda = config.Agenda{Prefix: "D", First: 1}
	cfg.Runtime.Daemon.RebuildIntervalSeconds = 60
	f := &cfg.Project.Fields
	f.Status.Name, f.Kind.Name, f.Agenda.Name = "Status", "Kind", "Agenda"
	f.Status.Options = config.StatusOptions{Todo: "Todo", InProgress: "In Progress", Done: "Done", Superseded: "Superseded"}
	f.Kind.Options = config.KindOptions{Human: "Human", Agenda: "Agenda", Implementation: "Implementation"}
	cfg.Seats = []config.Seat{{ID: "A", Rights: []config.Right{config.RightVote}}}
	return cfg
}

var project = github.Project{
	ID:     "P",
	Status: github.Select{ID: "S", Options: map[string]string{"Todo": "todo-id"}},
	Agenda: "AG",
}

func TestAdmission(t *testing.T) {
	gh := &fakeGitHub{
		items: []github.Item{
			{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D3", "Status": "Done"}},
			{ID: "i4", Repo: "o/pm", Number: 4, Fields: map[string]string{"Kind": "Human"}},
			{ID: "i2", Repo: "o/pm", Number: 2, Fields: map[string]string{"Kind": "Agenda"}},
			{ID: "i3", Repo: "o/pm", Number: 3, Fields: map[string]string{}}, // no Kind: not admitted
			{ID: "i5", Repo: "o/pm", Number: 5, Fields: map[string]string{"Kind": "Implementation"}},
			{ID: "x9", Repo: "o/other", Number: 9, Fields: map[string]string{"Kind": "Agenda"}},
			{ID: "i6", Repo: "o/pm", Number: 6, Fields: map[string]string{"Kind": "Agenda", "Status": "Done"}}, // closed before admission
		},
		blockers: map[int][]github.IssueRef{4: {{Repo: "o/pm", Number: 1}}},
	}
	d, err := New(context.Background(), testConfig(), gh, project, &fakePanels{})
	if err != nil {
		t.Fatal(err)
	}
	want := []write{
		{"i2", "S", "todo-id"}, {"i2", "AG", "D4"},
		{"i4", "S", "todo-id"}, {"i4", "AG", "D5"},
		{"i6", "AG", "D6"},
	}
	if !reflect.DeepEqual(gh.writes, want) {
		t.Fatalf("writes = %v, want %v", gh.writes, want)
	}
	q := d.Queue()
	got := map[queue.Ref]string{}
	for _, e := range q {
		got[e.Ref] = *e.Agenda + " " + *e.Status + " " + string(e.State)
	}
	wantQ := map[queue.Ref]string{"o/pm#1": "D3 Done decided", "o/pm#2": "D4 Todo held", "o/pm#4": "D5 Todo ready", "o/pm#6": "D6 Done decided"}
	if !reflect.DeepEqual(got, wantQ) {
		t.Fatalf("queue = %v, want %v", got, wantQ)
	}
	if q[0].Ref != "o/pm#4" {
		t.Errorf("human not first: %v", q[0].Ref)
	}

	// A second rebuild of the now-admitted items writes nothing.
	gh.items[1].Fields = map[string]string{"Kind": "Human", "Agenda": "D5", "Status": "Todo"}
	gh.items[2].Fields = map[string]string{"Kind": "Agenda", "Agenda": "D4", "Status": "Todo"}
	gh.items[6].Fields = map[string]string{"Kind": "Agenda", "Agenda": "D6", "Status": "Done"}
	gh.writes = nil
	if err := d.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.admitAndPublish(context.Background())
	if len(gh.writes) != 0 {
		t.Fatalf("second rebuild wrote %v", gh.writes)
	}

	// Refreshing an issue that left the Project drops it.
	gh.items = gh.items[1:]
	if err := d.refresh(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	d.admitAndPublish(context.Background())
	if len(d.Queue()) != 3 {
		t.Fatalf("queue after refresh = %v", d.Queue())
	}
}

func TestScheduleInterruptsForHumanIssue(t *testing.T) {
	gh := &fakeGitHub{items: []github.Item{
		{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Todo"}},
		{ID: "i2", Repo: "o/pm", Number: 2, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D2", "Status": "Todo"}},
		{ID: "i3", Repo: "o/pm", Number: 3, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D3", "Status": "Todo"}},
	}}
	cfg := testConfig()
	cfg.Council.MaxConcurrentPanels = 2
	fp := &fakePanels{started: make(chan panel.Job, 8), ended: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, gh, project, fp)
	if err != nil {
		t.Fatal(err)
	}

	d.schedule(ctx, d.started)
	got := map[queue.Ref]queue.Start{}
	for range 2 {
		j := <-fp.started
		got[j.Ref] = j.Start
	}
	if want := map[queue.Ref]queue.Start{"o/pm#1": queue.StartConvene, "o/pm#2": queue.StartConvene}; !reflect.DeepEqual(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	if len(d.live) != 2 {
		t.Fatalf("live = %d", len(d.live))
	}

	// A human issue arrives: both agenda panels are interrupted before the human one starts.
	gh.items = append(gh.items, github.Item{ID: "i4", Repo: "o/pm", Number: 4, Fields: map[string]string{"Kind": "Human", "Agenda": "D4", "Status": "Todo"}})
	if err := d.refresh(ctx, 4); err != nil {
		t.Fatal(err)
	}
	d.admitAndPublish(ctx)
	d.schedule(ctx, time.Time{})
	for range 2 {
		if cause := <-fp.ended; !errors.Is(cause, panel.ErrInterrupted) {
			t.Errorf("cause = %v", cause)
		}
	}
	j := <-fp.started
	if j.Ref != "o/pm#4" || j.Start != queue.StartConvene || len(j.HumanIssues) != 1 || j.HumanIssues[0].Decided {
		t.Fatalf("human job = %+v", j)
	}
	if len(d.live) != 1 || d.live[4] == nil || !d.live[4].human {
		t.Fatalf("live after interrupt = %v", d.live)
	}
}

// A human issue's panel ends after superseding an agenda issue: the daemon re-reads the
// superseded issue before scheduling, so the agenda issue, no longer held, is not convened from
// the copy read before the supersede.
func TestPanelEndRefreshesSupersededIssues(t *testing.T) {
	gh := &fakeGitHub{items: []github.Item{
		{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Todo"}},
		{ID: "i2", Repo: "o/pm", Number: 2, Fields: map[string]string{"Kind": "Human", "Agenda": "D2", "Status": "In Progress"}},
	}}
	cfg := testConfig()
	cfg.Council.MaxConcurrentPanels = 1
	fp := &fakePanels{started: make(chan panel.Job, 8), ended: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, gh, project, fp)
	if err != nil {
		t.Fatal(err)
	}
	if st := entryState(d.Queue(), "o/pm#1"); st != queue.StateHeld {
		t.Fatalf("agenda issue before the decision = %q, want held", st)
	}

	// The human panel posts its decision and supersedes #1 on GitHub; the copy still has #1 as Todo.
	decision, err := markers.Comment("", markers.Decision{
		Version:    1,
		Rows:       []markers.DecisionRow{{Row: "r1", Outcome: markers.OutcomePassed, Approve: 1, Threshold: 1, Reason: "approve 1 meets threshold 1"}},
		Impl:       []string{},
		Superseded: []string{"o/pm#1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mark, err := markers.Comment("", markers.Superseded{By: "o/pm#2"})
	if err != nil {
		t.Fatal(err)
	}
	gh.comments = map[int][]github.Comment{1: {{Body: mark}}, 2: {{Body: decision}}}
	gh.items[0].Fields = map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Superseded"}
	gh.items[1].Fields = map[string]string{"Kind": "Human", "Agenda": "D2", "Status": "Done"}

	if err := d.refreshDecided(ctx, 2); err != nil {
		t.Fatal(err)
	}
	d.admitAndPublish(ctx)
	d.schedule(ctx, time.Time{})
	if len(d.live) != 0 {
		t.Fatalf("panels started after the supersede: %v", d.live)
	}
	if st := entryState(d.Queue(), "o/pm#1"); st != queue.StateSuperseded {
		t.Fatalf("agenda issue after the decision = %q, want superseded", st)
	}
}

func entryState(q queue.Queue, ref queue.Ref) queue.State {
	for _, e := range q {
		if e.Ref == ref {
			return e.State
		}
	}
	return ""
}

// A failed panel is resumed by a full rebuild only when its pm:panel failed comment predates the
// daemon process; refreshes resume none.
func TestFailedResumesOnlyFromAnEarlierProcess(t *testing.T) {
	reason := "model not found"
	failed, err := markers.Comment("failed", markers.Panel{Event: markers.PanelFailed, Round: 1, Sessions: []markers.PanelSession{}, Error: &reason})
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{
		items: []github.Item{
			{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "In Progress"}},
			{ID: "i2", Repo: "o/pm", Number: 2, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D2", "Status": "In Progress"}},
		},
		comments: map[int][]github.Comment{
			1: {{Body: failed, CreatedAt: time.Now().Add(-time.Hour)}},
			2: {{Body: "a note"}, {Body: failed, CreatedAt: time.Now().Add(time.Hour)}, {Body: "after the marker"}},
		},
	}
	fp := &fakePanels{started: make(chan panel.Job, 8), ended: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig()
	cfg.Council.MaxConcurrentPanels = 2
	d, err := New(ctx, cfg, gh, project, fp)
	if err != nil {
		t.Fatal(err)
	}
	d.schedule(ctx, time.Time{})
	if len(d.live) != 0 {
		t.Fatalf("refresh resumed %v", d.live)
	}
	d.schedule(ctx, d.started)
	if j := <-fp.started; j.Ref != "o/pm#1" || j.Start != queue.StartResume {
		t.Fatalf("job = %+v", j)
	}
	if len(d.live) != 1 || d.live[1] == nil {
		t.Fatalf("live = %v", d.live)
	}
}

func TestScheduleStopsPanelsOfIssuesThatLeftDeliberation(t *testing.T) {
	gh := &fakeGitHub{items: []github.Item{
		{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Todo"}},
		{ID: "i2", Repo: "o/pm", Number: 2, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D2", "Status": "Todo"}},
		{ID: "i3", Repo: "o/pm", Number: 3, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D3", "Status": "Todo"}},
	}}
	cfg := testConfig()
	cfg.Council.MaxConcurrentPanels = 2
	fp := &fakePanels{started: make(chan panel.Job, 8), ended: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, gh, project, fp)
	if err != nil {
		t.Fatal(err)
	}
	d.schedule(ctx, d.started)
	for range 2 {
		<-fp.started
	}
	step := func(n int, resumeBefore time.Time) {
		t.Helper()
		if err := d.refresh(ctx, n); err != nil {
			t.Fatal(err)
		}
		d.admitAndPublish(ctx)
		d.schedule(ctx, resumeBefore)
	}
	stoppedBecause := func(want error) {
		t.Helper()
		if cause := <-fp.ended; !errors.Is(cause, want) {
			t.Fatalf("cause = %v, want %v", cause, want)
		}
	}

	// Closing #1 stops its panel and frees the slot for #3.
	gh.items[0].Closed = true
	step(1, time.Time{})
	stoppedBecause(panel.ErrLeftDeliberation)
	if j := <-fp.started; j.Ref != "o/pm#3" {
		t.Fatalf("started %v", j.Ref)
	}

	// Setting #2 to Done stops its panel; a full rebuild convenes neither #1 nor #2 again.
	gh.items[1].Fields["Status"] = "Done"
	step(2, d.started)
	stoppedBecause(panel.ErrLeftDeliberation)
	if len(d.live) != 1 || d.live[3] == nil {
		t.Fatalf("live = %v", d.live)
	}

	// A human issue interrupts #3; closing it undecided stops its panel and #3 is convened again.
	gh.items = append(gh.items, github.Item{ID: "i4", Repo: "o/pm", Number: 4, Fields: map[string]string{"Kind": "Human", "Agenda": "D4", "Status": "Todo"}})
	step(4, time.Time{})
	stoppedBecause(panel.ErrInterrupted)
	if j := <-fp.started; j.Ref != "o/pm#4" {
		t.Fatalf("started %v", j.Ref)
	}
	gh.items[3].Closed = true
	step(4, time.Time{})
	stoppedBecause(panel.ErrLeftDeliberation)
	if j := <-fp.started; j.Ref != "o/pm#3" {
		t.Fatalf("started %v", j.Ref)
	}
	if len(d.live) != 1 || d.live[3] == nil {
		t.Fatalf("live = %v", d.live)
	}
}

// The panel's own publish closes the issue before setting Status=Done; the issues closed event
// that follows must not stop the panel that is still publishing, while a closed issue without a
// pm:decision still has its panel stopped.
func TestScheduleKeepsThePanelPublishingItsDecision(t *testing.T) {
	gh := &fakeGitHub{items: []github.Item{
		{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Todo"}},
		{ID: "i2", Repo: "o/pm", Number: 2, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D2", "Status": "Todo"}},
	}}
	cfg := testConfig()
	cfg.Council.MaxConcurrentPanels = 2
	fp := &fakePanels{started: make(chan panel.Job, 8), ended: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, cfg, gh, project, fp)
	if err != nil {
		t.Fatal(err)
	}
	d.schedule(ctx, d.started)
	for range 2 {
		<-fp.started
	}

	decision, err := markers.Comment("", markers.Decision{
		Version:    1,
		Rows:       []markers.DecisionRow{{Row: "r1", Outcome: markers.OutcomePassed, Approve: 1, Threshold: 1, Reason: "approve 1 meets threshold 1"}},
		Impl:       []string{},
		Superseded: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	gh.comments = map[int][]github.Comment{1: {{Body: decision}}}
	gh.items[0].Closed = true
	gh.items[0].Fields["Status"] = "In Progress"
	gh.items[1].Closed = true
	for _, n := range []int{1, 2} {
		if err := d.refresh(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	d.admitAndPublish(ctx)
	d.schedule(ctx, time.Time{})
	if cause := <-fp.ended; !errors.Is(cause, panel.ErrLeftDeliberation) {
		t.Fatalf("cause = %v, want %v", cause, panel.ErrLeftDeliberation)
	}
	if len(d.live) != 1 || d.live[1] == nil {
		t.Fatalf("live = %v, want only the publishing panel of o/pm#1", d.live)
	}
	select {
	case cause := <-fp.ended:
		t.Fatalf("the publishing panel ended: %v", cause)
	default:
	}
}

// An issue whose updatedAt did not move since the last copy is not re-read: its record is kept
// with Kind/Status/Agenda and Closed taken from the fresh ProjectItems values (those writes do
// not move updatedAt). A moved updatedAt or an issue the copy has not seen is read again.
func TestRebuildReusesUnchangedIssues(t *testing.T) {
	t1 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	item := func(n int, updatedAt time.Time) github.Item {
		return github.Item{
			ID: fmt.Sprintf("i%d", n), Repo: "o/pm", Number: n,
			UpdatedAt: updatedAt,
			Fields:    map[string]string{"Kind": "Agenda", "Agenda": fmt.Sprintf("D%d", n), "Status": "Todo"},
		}
	}
	gh := &fakeGitHub{
		items:         []github.Item{item(1, t1), item(2, t1)},
		comments:      map[int][]github.Comment{},
		blockers:      map[int][]github.IssueRef{},
		commentsCalls: map[int]int{},
		blockedCalls:  map[int]int{},
	}
	d, err := New(context.Background(), testConfig(), gh, project, &fakePanels{})
	if err != nil {
		t.Fatal(err)
	}
	gh.commentsCalls = map[int]int{}
	gh.blockedCalls = map[int]int{}

	// case 1: no change since the last copy: no REST call at all.
	gh.items[0].Fields["Status"] = "Done" // a Project field write does not move updatedAt
	if err := d.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.commentsCalls) != 0 || len(gh.blockedCalls) != 0 {
		t.Fatalf("rebuild made REST calls: comments %v blocked %v", gh.commentsCalls, gh.blockedCalls)
	}
	r := d.records[1]
	if r.issue.Status != queue.StatusDone || !r.updatedAt.Equal(t1) || r.itemID != "i1" {
		t.Fatalf("reused record not patched from the fresh item: %+v", r)
	}

	// case 2: updatedAt moved: the issue is read again.
	gh.items[1].UpdatedAt = t1.Add(time.Minute)
	gh.comments[2] = []github.Comment{{Body: "hello", CreatedAt: t1}}
	resetCalls(gh)
	if err := d.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gh.commentsCalls[2] != 1 || gh.blockedCalls[2] != 1 {
		t.Fatalf("reread of changed issue wrong: comments %v blocked %v", gh.commentsCalls, gh.blockedCalls)
	}
	if gh.commentsCalls[1] != 0 || gh.blockedCalls[1] != 0 {
		t.Fatalf("unchanged issue was reread: comments %v blocked %v", gh.commentsCalls, gh.blockedCalls)
	}
	if len(d.records[2].issue.Markers) != 0 { // hello is not a marker
		t.Fatalf("unexpected markers: %v", d.records[2].issue.Markers)
	}

	// case 3: a comment marker arrives: the reread record carries it.
	panelMarker, err := markers.Comment("", markers.Panel{Event: markers.PanelStarted, Round: 1, Sessions: []markers.PanelSession{}})
	if err != nil {
		t.Fatal(err)
	}
	gh.items[1].UpdatedAt = t1.Add(2 * time.Minute)
	gh.comments[2] = []github.Comment{{Body: panelMarker, CreatedAt: t1.Add(time.Minute)}}
	if err := d.rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.records[2].issue.Markers) != 1 {
		t.Fatalf("marker lost: %v", d.records[2].issue.Markers)
	}
}

// Startup rebuild (New), refresh and refreshDecided read every issue they touch, whatever the
// updatedAt: the startup has no copy; a panel has just changed the issue on GitHub.
func TestFullReadsAreNotIncremental(t *testing.T) {
	t1 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	it := func(n int) github.Item {
		return github.Item{
			ID:        fmt.Sprintf("i%d", n),
			Repo:      "o/pm",
			Number:    n,
			UpdatedAt: t1,
			Fields:    map[string]string{"Kind": "Agenda", "Agenda": fmt.Sprintf("D%d", n), "Status": "In Progress"},
		}
	}
	gh := &fakeGitHub{
		items:         []github.Item{it(1), it(2), it(3)},
		comments:      map[int][]github.Comment{},
		blockers:      map[int][]github.IssueRef{},
		commentsCalls: map[int]int{},
		blockedCalls:  map[int]int{},
	}
	d, err := New(context.Background(), testConfig(), gh, project, &fakePanels{})
	if err != nil {
		t.Fatal(err)
	}
	if gh.commentsCalls[1] != 1 || gh.commentsCalls[2] != 1 || gh.commentsCalls[3] != 1 ||
		gh.blockedCalls[1] != 1 || gh.blockedCalls[2] != 1 || gh.blockedCalls[3] != 1 {
		t.Fatalf("startup rebuild was not a full read: comments %v blocked %v", gh.commentsCalls, gh.blockedCalls)
	}
	resetCalls(gh)

	// A single-issue refresh of an issue whose updatedAt is unchanged still reads it fully.
	if err := d.refresh(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if gh.commentsCalls[2] != 1 || gh.blockedCalls[2] != 1 {
		t.Fatalf("refresh did not read the issue: comments %v blocked %v", gh.commentsCalls, gh.blockedCalls)
	}
	resetCalls(gh)

	// refreshDecided reads the issue its panel ended on.
	if err := d.refreshDecided(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if gh.commentsCalls[3] != 1 || gh.blockedCalls[3] != 1 {
		t.Fatalf("refreshDecided did not read the issue: comments %v blocked %v", gh.commentsCalls, gh.blockedCalls)
	}
}

func resetCalls(gh *fakeGitHub) {
	gh.commentsCalls = map[int]int{}
	gh.blockedCalls = map[int]int{}
}
