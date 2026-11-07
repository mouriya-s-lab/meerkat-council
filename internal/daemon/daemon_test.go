package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"

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
	comments map[int][]string
	blockers map[int][]github.IssueRef
	writes   []write
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

func (f *fakeGitHub) Comments(_ context.Context, _ string, n int) ([]string, error) {
	return f.comments[n], nil
}

func (f *fakeGitHub) BlockedBy(_ context.Context, _ string, n int) ([]github.IssueRef, error) {
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
	}
	if !reflect.DeepEqual(gh.writes, want) {
		t.Fatalf("writes = %v, want %v", gh.writes, want)
	}
	q := d.Queue()
	got := map[queue.Ref]string{}
	for _, e := range q {
		got[e.Ref] = *e.Agenda + " " + *e.Status + " " + string(e.State)
	}
	wantQ := map[queue.Ref]string{"o/pm#1": "D3 Done decided", "o/pm#2": "D4 Todo held", "o/pm#4": "D5 Todo ready"}
	if !reflect.DeepEqual(got, wantQ) {
		t.Fatalf("queue = %v, want %v", got, wantQ)
	}
	if q[0].Ref != "o/pm#4" {
		t.Errorf("human not first: %v", q[0].Ref)
	}

	// A second rebuild of the now-admitted items writes nothing.
	gh.items[1].Fields = map[string]string{"Kind": "Human", "Agenda": "D5", "Status": "Todo"}
	gh.items[2].Fields = map[string]string{"Kind": "Agenda", "Agenda": "D4", "Status": "Todo"}
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
	if len(d.Queue()) != 2 {
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

	d.schedule(ctx, true)
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
	d.schedule(ctx, false)
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
	gh.comments = map[int][]string{1: {mark}, 2: {decision}}
	gh.items[0].Fields = map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Superseded"}
	gh.items[1].Fields = map[string]string{"Kind": "Human", "Agenda": "D2", "Status": "Done"}

	if err := d.refreshDecided(ctx, 2); err != nil {
		t.Fatal(err)
	}
	d.admitAndPublish(ctx)
	d.schedule(ctx, false)
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
