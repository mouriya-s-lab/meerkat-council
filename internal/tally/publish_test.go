package tally

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// errCrash is the fake's kill -9: the mutation that hits the budget is not applied and every
// later call fails.
var errCrash = errors.New("crashed")

type fakeIssue struct {
	repo     string
	number   int
	title    string
	body     string
	closed   bool
	reason   github.CloseReason
	comments []string
	parent   string
	item     map[string]string // Project field values by field name; nil when not in the Project
	itemID   string
}

type fakeGitHub struct {
	issues  map[string]*fakeIssue // by lower-case ref
	files   map[string]string     // PM repository default branch: path -> content
	commits int
	budget  int // mutations left before the crash; -1 never crashes
	crashed bool
	project github.Project
	fields  map[string]string // field id -> field name
	options map[string]string // option id -> option name
}

func key(repo string, n int) string { return strings.ToLower(fmt.Sprintf("%s#%d", repo, n)) }

func (f *fakeGitHub) issue(repo string, n int) (*fakeIssue, error) {
	is, ok := f.issues[key(repo, n)]
	if !ok {
		return nil, fmt.Errorf("no issue %s#%d", repo, n)
	}
	return is, nil
}

// mutate spends one unit of the crash budget.
func (f *fakeGitHub) mutate() error {
	if f.crashed {
		return errCrash
	}
	if f.budget == 0 {
		f.crashed = true
		return errCrash
	}
	if f.budget > 0 {
		f.budget--
	}
	return nil
}

func (f *fakeGitHub) read() error {
	if f.crashed {
		return errCrash
	}
	return nil
}

func (f *fakeGitHub) Issue(_ context.Context, repo string, n int) (github.Issue, error) {
	if err := f.read(); err != nil {
		return github.Issue{}, err
	}
	is, err := f.issue(repo, n)
	if err != nil {
		return github.Issue{}, err
	}
	return github.Issue{NodeID: "node-" + key(repo, n), Title: is.title, Body: is.body, Closed: is.closed}, nil
}

func (f *fakeGitHub) CommentMarkers(_ context.Context, repo string, n int, fn func(m markers.Marker) error) error {
	if err := f.read(); err != nil {
		return err
	}
	is, err := f.issue(repo, n)
	if err != nil {
		return err
	}
	for _, b := range is.comments {
		// Malformed markers are skipped; only fn errors abort.
		if m, err := markers.Parse(b); err == nil {
			if err := fn(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeGitHub) PostComment(_ context.Context, repo string, n int, body string) (int64, error) {
	if err := f.mutate(); err != nil {
		return 0, err
	}
	is, err := f.issue(repo, n)
	if err != nil {
		return 0, err
	}
	is.comments = append(is.comments, body)
	return int64(len(is.comments)), nil
}

func (f *fakeGitHub) EditIssueBody(_ context.Context, repo string, n int, body string) error {
	if err := f.mutate(); err != nil {
		return err
	}
	is, err := f.issue(repo, n)
	if err != nil {
		return err
	}
	is.body = body
	return nil
}

func (f *fakeGitHub) CloseIssue(_ context.Context, repo string, n int, reason github.CloseReason) error {
	if err := f.mutate(); err != nil {
		return err
	}
	is, err := f.issue(repo, n)
	if err != nil {
		return err
	}
	is.closed, is.reason = true, reason
	return nil
}

func (f *fakeGitHub) SubIssues(_ context.Context, repo string, n int) ([]github.SubIssue, error) {
	if err := f.read(); err != nil {
		return nil, err
	}
	var out []github.SubIssue
	for _, k := range slices.Sorted(maps.Keys(f.issues)) {
		is := f.issues[k]
		if is.parent == key(repo, n) {
			out = append(out, github.SubIssue{Repo: is.repo, Number: is.number, NodeID: "node-" + k, Title: is.title, Body: is.body})
		}
	}
	return out, nil
}

func (f *fakeGitHub) CreateIssue(_ context.Context, n github.NewIssue) (github.SubIssue, error) {
	if err := f.mutate(); err != nil {
		return github.SubIssue{}, err
	}
	num := 1
	for _, is := range f.issues {
		if strings.EqualFold(is.repo, n.Repo) && is.number >= num {
			num = is.number + 1
		}
	}
	is := &fakeIssue{repo: n.Repo, number: num, title: n.Title, body: n.Body, parent: strings.TrimPrefix(n.ParentID, "node-")}
	if n.ProjectID == f.project.ID {
		is.item, is.itemID = map[string]string{}, "item-"+key(n.Repo, num)
	}
	f.issues[key(n.Repo, num)] = is
	return github.SubIssue{Repo: n.Repo, Number: num, NodeID: "node-" + key(n.Repo, num), Title: n.Title, Body: n.Body}, nil
}

func (f *fakeGitHub) IssueItem(_ context.Context, repo string, n int, projectID string) (github.Item, bool, error) {
	if err := f.read(); err != nil {
		return github.Item{}, false, err
	}
	is, ok := f.issues[key(repo, n)]
	if !ok || is.item == nil || projectID != f.project.ID {
		return github.Item{}, false, nil
	}
	return github.Item{ID: is.itemID, Repo: is.repo, Number: is.number, Fields: maps.Clone(is.item)}, true, nil
}

func (f *fakeGitHub) AddProjectItem(_ context.Context, projectID, contentID string) (string, error) {
	if err := f.mutate(); err != nil {
		return "", err
	}
	is, ok := f.issues[strings.TrimPrefix(contentID, "node-")]
	if !ok {
		return "", fmt.Errorf("no content %s", contentID)
	}
	if is.item == nil {
		is.item, is.itemID = map[string]string{}, "item-"+key(is.repo, is.number)
	}
	return is.itemID, nil
}

func (f *fakeGitHub) SetOption(_ context.Context, projectID, itemID, fieldID, optionID string) error {
	if err := f.mutate(); err != nil {
		return err
	}
	for _, is := range f.issues {
		if is.itemID == itemID && is.item != nil {
			field, option := f.fields[fieldID], f.options[optionID]
			if field == "" || option == "" {
				return fmt.Errorf("unknown field %s or option %s", fieldID, optionID)
			}
			is.item[field] = option
			return nil
		}
	}
	return fmt.Errorf("no item %s", itemID)
}

func (f *fakeGitHub) DefaultBranch(context.Context, string) (string, error) { return "main", f.read() }

func (f *fakeGitHub) FileExists(_ context.Context, _, _, path string) (bool, error) {
	if err := f.read(); err != nil {
		return false, err
	}
	_, ok := f.files[path]
	return ok, nil
}

func (f *fakeGitHub) CommitFiles(_ context.Context, _, _, _ string, files map[string]string) (string, error) {
	if err := f.mutate(); err != nil {
		return "", err
	}
	maps.Copy(f.files, files)
	f.commits++
	return fmt.Sprintf("sha%d", f.commits), nil
}

// state renders everything a publish can change, for comparing runs.
func (f *fakeGitHub) state() string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(f.issues)) {
		is := f.issues[k]
		fmt.Fprintf(&b, "%s title=%q closed=%t reason=%s parent=%s item=%v\nbody=%q\n", k, is.title, is.closed, is.reason, is.parent, is.item, is.body)
		for _, c := range is.comments {
			fmt.Fprintf(&b, "  comment %q\n", c)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(f.files)) {
		fmt.Fprintf(&b, "file %s %q\n", p, f.files[p])
	}
	fmt.Fprintf(&b, "commits %d\n", f.commits)
	return b.String()
}

const (
	pmRepo   = "oh-my-omp/pm-e2e-agenda"
	codeRepo = "oh-my-omp/pm-e2e-code"
)

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config.example"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testProject(cfg *config.Config) (github.Project, map[string]string, map[string]string) {
	f := cfg.Project.Fields
	sel := func(id string, names []config.Named) github.Select {
		s := github.Select{ID: id, Options: map[string]string{}}
		for _, n := range names {
			s.Options[n.Name] = id + "-" + n.Key
		}
		return s
	}
	p := github.Project{
		ID:       "P",
		Status:   sel("F-status", f.Status.Options.List()),
		Kind:     sel("F-kind", f.Kind.Options.List()),
		Proposer: sel("F-proposer", f.Proposer.Options.List()),
		Agenda:   "F-agenda",
	}
	fields := map[string]string{"F-status": f.Status.Name, "F-kind": f.Kind.Name, "F-proposer": f.Proposer.Name}
	options := map[string]string{}
	for _, s := range []github.Select{p.Status, p.Kind, p.Proposer} {
		for name, id := range s.Options {
			options[id] = name
		}
	}
	return p, fields, options
}

func mustComment(t *testing.T, text string, m markers.Marker) string {
	t.Helper()
	c, err := markers.Comment(text, m)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// humanScenario: human issue #20 raised from ask 77 on code#3, final draft v1 with a passed row
// delivering to the code repository (it replaces code#3), a rejected row, and supersedes of an
// agenda issue (#21) and a human issue (#22, refused).
func humanScenario(t *testing.T, cfg *config.Config) (*fakeGitHub, Publication) {
	t.Helper()
	p, fields, options := testProject(cfg)
	fo := cfg.Project.Fields
	esc := mustComment(t, "raised from an ask", markers.Escalation{Issue: codeRepo + "#3", Ask: 77})
	draft := markers.Draft{
		Version: 1,
		Rows: []markers.Row{
			{ID: "D20-01", What: "count", Where: "main.go", How: "add", Tradeoff: "none", Category: "c", Deliver: &markers.Deliver{Repo: codeRepo, Title: "notes count", Body: "add notes count"}},
			{ID: "D20-02", What: "other", Where: "x", How: "y", Tradeoff: "z", Category: "c", Deliver: &markers.Deliver{Repo: codeRepo, Title: "other", Body: "b"}},
		},
		Supersedes: []string{pmRepo + "#21", pmRepo + "#22"},
		Settle:     &markers.Settle{Kind: markers.SettleReplace, Text: "replace it"},
	}
	vote := markers.Vote{Seat: "Argon", Version: 1, Rows: []markers.VoteRow{{Row: "D20-01", Choice: markers.ChoiceApprove}, {Row: "D20-02", Choice: markers.ChoiceApprove}}}
	gh := &fakeGitHub{
		budget: -1, project: p, fields: fields, options: options, files: map[string]string{},
		issues: map[string]*fakeIssue{
			key(pmRepo, 20): {repo: pmRepo, number: 20, title: "human", body: "please\n\n" + esc, itemID: "item-20",
				item:     map[string]string{fo.Kind.Name: fo.Kind.Options.Human, fo.Status.Name: fo.Status.Options.InProgress},
				comments: []string{mustComment(t, "draft", draft), mustComment(t, "vote", vote)}},
			key(pmRepo, 21): {repo: pmRepo, number: 21, title: "old", itemID: "item-21",
				item: map[string]string{fo.Kind.Name: fo.Kind.Options.Agenda, fo.Status.Name: fo.Status.Options.Todo}},
			key(pmRepo, 22): {repo: pmRepo, number: 22, title: "other human", itemID: "item-22",
				item: map[string]string{fo.Kind.Name: fo.Kind.Options.Human, fo.Status.Name: fo.Status.Options.Todo}},
			key(codeRepo, 3): {repo: codeRepo, number: 3, title: "impl", itemID: "item-c3",
				item:     map[string]string{fo.Kind.Name: fo.Kind.Options.Implementation, fo.Status.Name: fo.Status.Options.InProgress},
				comments: []string{"<!-- pm:ask {\"v\":1,\"tier\":\"complex\",\"session\":\"s\"} -->"}},
		},
	}
	rows := []markers.DecisionRow{
		{Row: "D20-01", Outcome: markers.OutcomePassed, Approve: 4, Threshold: 3, Reason: "approve 4 meets threshold 3"},
		{Row: "D20-02", Outcome: markers.OutcomeRejected, Approve: 2, Threshold: 3, Reason: "approve 2 below threshold 3"},
	}
	return gh, Publication{Ref: queue.Ref(pmRepo + "#20"), Agenda: "D20", Draft: draft, Rows: rows}
}

func markersOf(t *testing.T, bodies []string) []markers.Marker {
	t.Helper()
	var ms []markers.Marker
	for _, b := range bodies {
		if m, err := markers.Parse(b); err == nil {
			ms = append(ms, m)
		}
	}
	return ms
}

func TestPublishHumanReplace(t *testing.T) {
	cfg := loadConfig(t)
	cfg.Council.Archive = &config.Archive{Decisions: "decisions", Votes: "votes"}
	gh, pub := humanScenario(t, cfg)
	tl := New(cfg, gh, gh.project)
	if err := tl.Publish(context.Background(), pub); err != nil {
		t.Fatal(err)
	}
	fo := cfg.Project.Fields

	h := gh.issues[key(pmRepo, 20)]
	var decs []markers.Decision
	for _, m := range markersOf(t, h.comments) {
		if d, ok := m.(markers.Decision); ok {
			decs = append(decs, d)
		}
	}
	impl := codeRepo + "#4"
	if len(decs) != 1 || !slices.Equal(decs[0].Impl, []string{impl}) || !slices.Equal(decs[0].Superseded, []string{pmRepo + "#21"}) {
		t.Fatalf("decisions = %+v", decs)
	}
	for _, r := range decs[0].Rows {
		if !strings.HasSuffix(r.Reason, "; supersede "+pmRepo+"#22 refused: a human issue") {
			t.Errorf("row %s reason %q lacks the refusal", r.Row, r.Reason)
		}
	}
	if last := h.comments[len(h.comments)-1]; !strings.Contains(last, "未变更：supersede "+pmRepo+"#22 refused: a human issue") {
		t.Errorf("decision prose lacks the refusal line: %q", last)
	}
	if !h.closed || h.reason != github.Completed || h.item[fo.Status.Name] != fo.Status.Options.Done {
		t.Errorf("agenda closed=%t reason=%s item=%v", h.closed, h.reason, h.item)
	}

	ni := gh.issues[key(codeRepo, 4)]
	if ni == nil || ni.parent != key(pmRepo, 20) || ni.title != "notes count" {
		t.Fatalf("implementation issue = %+v", ni)
	}
	if ni.item[fo.Kind.Name] != fo.Kind.Options.Implementation || ni.item[fo.Status.Name] != fo.Status.Options.Todo || ni.item[fo.Proposer.Name] != fo.Proposer.Options.CouncilSeat {
		t.Errorf("implementation item = %v", ni.item)
	}
	m, err := markers.Parse(ni.body)
	if rep, ok := m.(markers.Replaces); err != nil || !ok || rep.Old != codeRepo+"#3" || rep.Ask == nil || *rep.Ask != 77 {
		t.Errorf("implementation body marker = %#v, %v", m, err)
	}
	link, _ := cfg.Council.LaunchLink.Link(codeRepo, 4, cfg.Repos.Code[0].Cwd)
	if !strings.Contains(ni.body, link) || !strings.Contains(ni.body, pmRepo+"#20") {
		t.Errorf("implementation body lacks the numbered link %s or the agenda: %q", link, ni.body)
	}
	if u, _ := url.Parse(link); u.Path != "/"+codeRepo+"/issues/4" {
		t.Errorf("link path %s", u.Path)
	}
	if gh.issues[key(codeRepo, 5)] != nil {
		t.Error("the rejected row got an implementation issue")
	}

	old := gh.issues[key(pmRepo, 21)]
	if !old.closed || old.reason != github.NotPlanned || old.item[fo.Status.Name] != fo.Status.Options.Superseded ||
		!slices.ContainsFunc(markersOf(t, old.comments), func(m markers.Marker) bool { return m == markers.Superseded{By: pmRepo + "#20"} }) {
		t.Errorf("superseded issue = %+v", old)
	}
	if hu := gh.issues[key(pmRepo, 22)]; hu.closed || len(hu.comments) != 0 {
		t.Errorf("refused human issue changed: %+v", hu)
	}

	c3 := gh.issues[key(codeRepo, 3)]
	ms := markersOf(t, c3.comments)
	if len(ms) != 1 || !c3.closed || c3.reason != github.NotPlanned || c3.item[fo.Status.Name] != fo.Status.Options.Superseded {
		t.Fatalf("escalated issue = %+v", c3)
	}
	if r, ok := ms[0].(markers.Replaced); !ok || r.By != impl || r.Ask == nil || *r.Ask != 77 {
		t.Errorf("replaced = %#v", ms[0])
	}

	if gh.commits != 1 || !strings.Contains(gh.files["decisions/D20.md"], "D20-01") || !strings.Contains(gh.files["votes/D20/Argon.md"], "Argon") || len(gh.files) != 2 {
		t.Errorf("archive commits=%d files=%v", gh.commits, slices.Sorted(maps.Keys(gh.files)))
	}

	before := gh.state()
	if err := tl.Publish(context.Background(), pub); err != nil {
		t.Fatal(err)
	}
	if after := gh.state(); after != before {
		t.Errorf("a second publish changed GitHub:\n%s\nvs\n%s", before, after)
	}
}

// TestPublishCrashReplay kills the publish at every mutation and finishes it with a second run;
// the end state must equal an uninterrupted publish.
func TestPublishCrashReplay(t *testing.T) {
	cfg := loadConfig(t)
	cfg.Council.Archive = &config.Archive{Decisions: "decisions", Votes: "votes"}
	ref, pub := humanScenario(t, cfg)
	if err := New(cfg, ref, ref.project).Publish(context.Background(), pub); err != nil {
		t.Fatal(err)
	}
	want := ref.state()
	for n := 0; ; n++ {
		gh, pub := humanScenario(t, cfg)
		gh.budget = n
		tl := New(cfg, gh, gh.project)
		err := tl.Publish(context.Background(), pub)
		if err == nil {
			if n == 0 {
				t.Fatal("publish made no mutation")
			}
			break
		}
		if !errors.Is(err, errCrash) {
			t.Fatalf("crash after %d mutations: %v", n, err)
		}
		gh.crashed, gh.budget = false, -1
		if err := tl.Publish(context.Background(), pub); err != nil {
			t.Fatalf("rerun after %d mutations: %v", n, err)
		}
		if got := gh.state(); got != want {
			t.Fatalf("rerun after a crash at mutation %d ended in\n%s\nwant\n%s", n, got, want)
		}
	}
}

func TestPublishWithdrawnReply(t *testing.T) {
	cfg := loadConfig(t)
	p, fields, options := testProject(cfg)
	fo := cfg.Project.Fields
	esc := mustComment(t, "raised", markers.Escalation{Issue: codeRepo + "#3", Ask: 5})
	draft := markers.Draft{Version: 2, Rows: []markers.Row{{ID: "D9-01", What: "w", Where: "p", How: "h", Tradeoff: "t", Category: "c",
		Deliver: &markers.Deliver{Repo: codeRepo, Title: "t", Body: "b"}}}, Supersedes: []string{pmRepo + "#21"},
		Settle: &markers.Settle{Kind: markers.SettleReplace, Text: "no longer needed"}, Withdrawn: new("dropped")}
	gh := &fakeGitHub{
		budget: -1, project: p, fields: fields, options: options, files: map[string]string{},
		issues: map[string]*fakeIssue{
			key(pmRepo, 9): {repo: pmRepo, number: 9, title: "agenda", body: esc, itemID: "item-9",
				item: map[string]string{fo.Kind.Name: fo.Kind.Options.Agenda, fo.Status.Name: fo.Status.Options.InProgress}},
			key(pmRepo, 21): {repo: pmRepo, number: 21, itemID: "item-21",
				item: map[string]string{fo.Kind.Name: fo.Kind.Options.Agenda, fo.Status.Name: fo.Status.Options.Todo}},
			key(codeRepo, 3): {repo: codeRepo, number: 3},
		},
	}
	rows := []markers.DecisionRow{{Row: "D9-01", Outcome: markers.OutcomeWithdrawn, Approve: 0, Threshold: 3, Reason: "dropped"}}
	pub := Publication{Ref: queue.Ref(pmRepo + "#9"), Agenda: "D9", Draft: draft, Rows: rows}
	tl := New(cfg, gh, gh.project)
	for range 2 {
		if err := tl.Publish(context.Background(), pub); err != nil {
			t.Fatal(err)
		}
	}
	if len(gh.issues) != 3 {
		t.Errorf("issues = %d; a withdrawn draft creates none", len(gh.issues))
	}
	c3 := gh.issues[key(codeRepo, 3)]
	ms := markersOf(t, c3.comments)
	if len(ms) != 1 || c3.closed {
		t.Fatalf("escalated issue = %+v", c3)
	}
	if r, ok := ms[0].(markers.Reply); !ok || r.Ask != 5 || r.Decision == nil || *r.Decision != pmRepo+"#9" ||
		!strings.Contains(c3.comments[0], "no longer needed") || !strings.Contains(c3.comments[0], "| D9-01 | withdrawn |") {
		t.Errorf("reply = %#v %q", ms[0], c3.comments[0])
	}
	if old := gh.issues[key(pmRepo, 21)]; old.closed || len(old.comments) != 0 {
		t.Errorf("a non-human issue superseded %+v", old)
	}
	a := gh.issues[key(pmRepo, 9)]
	ds := markersOf(t, a.comments)
	if len(ds) != 1 || !a.closed || a.item[fo.Status.Name] != fo.Status.Options.Done {
		t.Errorf("agenda = %+v", a)
	}
	if d, ok := ds[0].(markers.Decision); !ok || len(d.Impl) != 0 || len(d.Superseded) != 0 || d.Rows[0].Outcome != markers.OutcomeWithdrawn {
		t.Errorf("decision = %#v", ds[0])
	}
	if gh.commits != 0 {
		t.Errorf("archived without archive config")
	}
}

// TestPublishBlankDeliverField: a passed row whose deliver has a blank title fails the publish
// with an error naming the row and the field, before any GitHub write; an earlier valid row's
// issue must not exist either.
func TestPublishBlankDeliverField(t *testing.T) {
	cfg := loadConfig(t)
	p, fields, options := testProject(cfg)
	fo := cfg.Project.Fields
	draft := markers.Draft{
		Version: 1,
		Rows: []markers.Row{
			{ID: "D5-01", What: "fine", Where: "x", How: "y", Tradeoff: "z", Category: "c", Deliver: &markers.Deliver{Repo: codeRepo, Title: "fine", Body: "b"}},
			{ID: "D5-02", What: "blank", Where: "x", How: "y", Tradeoff: "z", Category: "c", Deliver: &markers.Deliver{Repo: codeRepo, Title: "  ", Body: "b"}},
		},
		Supersedes: []string{},
	}
	vote := markers.Vote{Seat: "Argon", Version: 1, Rows: []markers.VoteRow{{Row: "D5-01", Choice: markers.ChoiceApprove}, {Row: "D5-02", Choice: markers.ChoiceApprove}}}
	gh := &fakeGitHub{
		budget: -1, project: p, fields: fields, options: options, files: map[string]string{},
		issues: map[string]*fakeIssue{
			key(pmRepo, 5): {repo: pmRepo, number: 5, title: "agenda", body: "needs work", itemID: "item-5",
				item:     map[string]string{fo.Kind.Name: fo.Kind.Options.Agenda, fo.Status.Name: fo.Status.Options.InProgress},
				comments: []string{mustComment(t, "draft", draft), mustComment(t, "vote", vote)}},
		},
	}
	rows := []markers.DecisionRow{
		{Row: "D5-01", Outcome: markers.OutcomePassed, Approve: 4, Threshold: 3, Reason: "approve 4 meets threshold 3"},
		{Row: "D5-02", Outcome: markers.OutcomePassed, Approve: 4, Threshold: 3, Reason: "approve 4 meets threshold 3"},
	}
	pub := Publication{Ref: queue.Ref(pmRepo + "#5"), Agenda: "D5", Draft: draft, Rows: rows}
	tl := New(cfg, gh, gh.project)
	err := tl.Publish(context.Background(), pub)
	if err == nil {
		t.Fatal("publish succeeded with a blank deliver.title")
	}
	if !strings.Contains(err.Error(), "D5-02") || !strings.Contains(err.Error(), "deliver.title") {
		t.Errorf("error %q does not name the row and the field", err.Error())
	}
	before := gh.state()
	if gh.issues[key(codeRepo, 1)] != nil {
		t.Error("the earlier valid row created an implementation issue before the failure")
	}
	if len(markersOf(t, gh.issues[key(pmRepo, 5)].comments)) != 2 {
		t.Error("the failed publish wrote a marker")
	}
	if err := tl.Publish(context.Background(), pub); err == nil || !strings.Contains(err.Error(), "deliver.title") {
		t.Errorf("a repeat run did not fail the same way: %v", err)
	}
	if after := gh.state(); after != before {
		t.Errorf("the failed runs changed GitHub:\n%s\nvs\n%s", before, after)
	}
}
