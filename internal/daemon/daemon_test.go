package daemon

import (
	"context"
	"reflect"
	"testing"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

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
	d, err := New(context.Background(), testConfig(), gh, project)
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
