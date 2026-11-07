// Package daemon keeps an in-memory copy of the PM repository's agenda issues, rebuilt from
// GitHub at startup, on every forwarded issue event (that one issue) and every rebuild interval
// (everything). It admits new agenda issues (Agenda number, Status=Todo), publishes the queue and
// starts, interrupts and resumes panels. GitHub is the only source of issue state; the copy is
// always replaceable by a rebuild. The live panels are processes of this daemon, not a copy.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/panel"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// GitHub is the part of the GitHub client the daemon uses.
type GitHub interface {
	ProjectItems(ctx context.Context, projectID string) ([]github.Item, error)
	IssueItem(ctx context.Context, repo string, number int, projectID string) (github.Item, bool, error)
	IssueComments(ctx context.Context, repo string, number int) ([]github.Comment, error)
	BlockedBy(ctx context.Context, repo string, number int) ([]github.IssueRef, error)
	SetText(ctx context.Context, projectID, itemID, fieldID, text string) error
	SetOption(ctx context.Context, projectID, itemID, fieldID, optionID string) error
}

// Panels runs one panel until it ends or ctx is cancelled (internal/panel).
type Panels interface {
	Run(ctx context.Context, j panel.Job)
}

// record is one PM-repository issue in the Project as last read from GitHub.
type record struct {
	itemID string
	issue  queue.Issue
}

// livePanel is a panel goroutine started by this daemon. done is closed when it has ended,
// including its interrupted post.
type livePanel struct {
	number int
	ref    queue.Ref
	human  bool
	cancel context.CancelCauseFunc
	done   chan struct{}
}

type Daemon struct {
	cfg      *config.Config
	gh       GitHub
	project  github.Project
	panels   Panels
	started  time.Time
	events   chan int
	finished chan *livePanel
	queue    atomic.Pointer[queue.Queue]

	// owned by the worker goroutine (Run) after New's initial rebuild
	records map[int]record
	live    map[int]*livePanel
}

// New records the process start time, performs the startup full rebuild (with admission) and
// returns the daemon ready to Run.
func New(ctx context.Context, cfg *config.Config, gh GitHub, project github.Project, panels Panels) (*Daemon, error) {
	d := &Daemon{
		cfg: cfg, gh: gh, project: project, panels: panels, started: time.Now(),
		events: make(chan int, 64), finished: make(chan *livePanel),
		records: map[int]record{}, live: map[int]*livePanel{},
	}
	if err := d.rebuild(ctx); err != nil {
		return nil, fmt.Errorf("startup rebuild: %w", err)
	}
	d.admitAndPublish(ctx)
	return d, nil
}

// Queue returns the latest computed queue.
func (d *Daemon) Queue() queue.Queue {
	if q := d.queue.Load(); q != nil {
		return *q
	}
	return queue.Queue{}
}

// Notify hands an issue number of the PM repository to the worker.
func (d *Daemon) Notify(ctx context.Context, number int) error {
	select {
	case d.events <- number:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run serves events, panel completions and periodic full rebuilds until ctx ends, then waits for
// every live panel to end. The startup rebuild done by New counts as a full rebuild. Full rebuilds
// resume failed and orphaned panels whose latest pm:panel predates the process start; refreshes
// resume none of them.
func (d *Daemon) Run(ctx context.Context) {
	tick := time.NewTicker(d.cfg.Runtime.Daemon.RebuildInterval())
	defer tick.Stop()
	defer d.waitPanels()
	d.schedule(ctx, d.started)
	for {
		var resumeBefore time.Time
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := d.rebuild(ctx); err != nil {
				slog.Error("full rebuild failed; keeping the previous copy", "err", err)
				continue
			}
			slog.Info("full rebuild", "issues", len(d.records))
			resumeBefore = d.started
		case n := <-d.events:
			if err := d.refresh(ctx, n); err != nil {
				slog.Error("issue refresh failed", "issue", n, "err", err)
				continue
			}
		case lp := <-d.finished:
			if d.live[lp.number] == lp {
				delete(d.live, lp.number)
			}
			slog.Info("panel ended", "issue", lp.ref)
			if err := d.refreshDecided(ctx, lp.number); err != nil {
				slog.Error("issue refresh failed", "issue", lp.number, "err", err)
				continue
			}
		}
		d.admitAndPublish(ctx)
		d.schedule(ctx, resumeBefore)
	}
}

// waitPanels waits for every live panel; their contexts end with the daemon's.
func (d *Daemon) waitPanels() {
	for _, lp := range d.live {
		<-lp.done
	}
}

// refreshDecided re-reads an issue whose panel ended and, when it carries a decision, every PM
// issue that decision superseded: the panel changed them on GitHub, and scheduling from the old
// copy would convene an issue that was just superseded.
func (d *Daemon) refreshDecided(ctx context.Context, number int) error {
	if err := d.refresh(ctx, number); err != nil {
		return err
	}
	r, ok := d.records[number]
	if !ok {
		return nil
	}
	dec, ok := markers.LastDecision(r.issue.Markers)
	if !ok {
		return nil
	}
	for _, ref := range dec.Superseded {
		repo, n, ok := queue.ParseRef(ref)
		if !ok || !strings.EqualFold(repo, d.cfg.Repos.PM.Repo) {
			continue
		}
		if err := d.refresh(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// schedule applies the panel rule after a published queue: while a human issue is undecided,
// interrupt every live panel of a non-human issue and wait until each has posted; then start
// panels for the queue's candidates (queue.Candidates with resumeBefore) up to
// maxConcurrentPanels.
func (d *Daemon) schedule(ctx context.Context, resumeBefore time.Time) {
	q := d.Queue()
	if queue.HumanPending(q) {
		var stopped []*livePanel
		for n, lp := range d.live {
			if !lp.human {
				slog.Info("interrupting panel for a human issue", "issue", lp.ref)
				lp.cancel(panel.ErrInterrupted)
				stopped = append(stopped, lp)
				delete(d.live, n)
			}
		}
		for _, lp := range stopped {
			<-lp.done
		}
	}
	live := map[queue.Ref]bool{}
	for _, lp := range d.live {
		live[lp.ref] = true
	}
	var humans []panel.HumanIssue
	for _, e := range q {
		if e.Human() {
			humans = append(humans, panel.HumanIssue{Ref: e.Ref, Decided: e.State == queue.StateDecided})
		}
	}
	slots := d.cfg.Council.MaxConcurrentPanels - len(d.live)
	for _, c := range queue.Candidates(q, live, resumeBefore) {
		if slots <= 0 {
			return
		}
		_, n, ok := queue.ParseRef(string(c.Ref))
		r, known := d.records[n]
		if !ok || !known {
			continue
		}
		d.start(ctx, r, c.Start, humans)
		slots--
	}
}

func (d *Daemon) start(ctx context.Context, r record, start queue.Start, humans []panel.HumanIssue) {
	pctx, cancel := context.WithCancelCause(ctx)
	lp := &livePanel{number: r.issue.Number, ref: r.issue.Ref, human: r.issue.Kind == queue.KindHuman, cancel: cancel, done: make(chan struct{})}
	d.live[lp.number] = lp
	job := panel.Job{
		Ref: r.issue.Ref, ItemID: r.itemID, Agenda: r.issue.Agenda, Start: start,
		HumanIssues: slices.Clone(humans),
	}
	slog.Info("panel starting", "issue", lp.ref, "start", start)
	go func() {
		defer cancel(nil)
		d.panels.Run(pctx, job)
		close(lp.done)
		select {
		case d.finished <- lp:
		case <-ctx.Done():
		}
	}()
}

// rebuild re-reads every PM-repository issue in the Project.
func (d *Daemon) rebuild(ctx context.Context) error {
	items, err := d.gh.ProjectItems(ctx, d.project.ID)
	if err != nil {
		return err
	}
	records := map[int]record{}
	for _, it := range items {
		if !strings.EqualFold(it.Repo, d.cfg.Repos.PM.Repo) {
			continue
		}
		r, err := d.read(ctx, it)
		if err != nil {
			return err
		}
		records[it.Number] = r
	}
	d.records = records
	return nil
}

// refresh re-reads one issue; an issue no longer in the Project leaves the copy.
func (d *Daemon) refresh(ctx context.Context, number int) error {
	it, ok, err := d.gh.IssueItem(ctx, d.cfg.Repos.PM.Repo, number, d.project.ID)
	if err != nil {
		return err
	}
	if !ok {
		delete(d.records, number)
		slog.Info("issue refreshed: not in the Project", "issue", number)
		return nil
	}
	r, err := d.read(ctx, it)
	if err != nil {
		return err
	}
	d.records[number] = r
	slog.Info("issue refreshed", "issue", r.issue.Ref)
	return nil
}

// read completes a Project item with the issue's comment markers and Blocked by relations.
func (d *Daemon) read(ctx context.Context, it github.Item) (record, error) {
	p := d.cfg.Project.Fields
	ref := queue.FormatRef(it.Repo, it.Number)
	is := queue.Issue{
		Ref:        ref,
		Number:     it.Number,
		Kind:       kindOf(p.Kind.Options, it.Fields[p.Kind.Name]),
		Status:     statusOf(p.Status.Options, it.Fields[p.Status.Name]),
		StatusName: it.Fields[p.Status.Name],
		Agenda:     it.Fields[p.Agenda.Name],
	}
	if is.Kind == queue.KindNone {
		slog.Info("issue has no Kind; not on the agenda", "issue", ref, "field", p.Kind.Name, "value", it.Fields[p.Kind.Name])
	}
	comments, err := d.gh.IssueComments(ctx, it.Repo, it.Number)
	if err != nil {
		return record{}, err
	}
	// The markers as markers.Collect gives them, and the time of the comment of the latest pm:panel.
	for _, c := range comments {
		m, err := markers.Parse(c.Body)
		switch {
		case errors.Is(err, markers.ErrNoMarker):
		case err != nil:
			slog.Error("ignoring malformed marker", "issue", ref, "err", err)
		default:
			is.Markers = append(is.Markers, m)
			if m.Kind() == markers.KindPanel {
				is.PanelAt = c.CreatedAt
			}
		}
	}
	blockers, err := d.gh.BlockedBy(ctx, it.Repo, it.Number)
	if err != nil {
		return record{}, err
	}
	for _, b := range blockers {
		is.BlockedBy = append(is.BlockedBy, queue.FormatRef(b.Repo, b.Number))
	}
	return record{itemID: it.ID, issue: is}, nil
}

func kindOf(o config.KindOptions, name string) queue.Kind {
	switch name {
	case "":
		return queue.KindNone
	case o.Human:
		return queue.KindHuman
	case o.Agenda:
		return queue.KindAgenda
	case o.Implementation:
		return queue.KindImplementation
	}
	return queue.KindNone
}

func statusOf(o config.StatusOptions, name string) queue.Status {
	switch name {
	case "":
		return queue.StatusNone
	case o.Todo:
		return queue.StatusTodo
	case o.InProgress:
		return queue.StatusInProgress
	case o.Done:
		return queue.StatusDone
	case o.Superseded:
		return queue.StatusSuperseded
	}
	return queue.StatusNone
}

// admitAndPublish admits agenda issues without an Agenda number, then publishes the queue.
func (d *Daemon) admitAndPublish(ctx context.Context) {
	if err := d.admit(ctx); err != nil {
		slog.Error("admission failed", "err", err)
	}
	snap := queue.Snapshot{AgendaPrefix: d.cfg.Council.Agenda.Prefix}
	for _, s := range d.cfg.Seats {
		snap.Seats = append(snap.Seats, queue.Seat{ID: s.ID, CanVote: s.Has(config.RightVote)})
	}
	for _, r := range d.records {
		snap.Issues = append(snap.Issues, r.issue)
	}
	q := queue.Build(snap)
	d.queue.Store(&q)
}

// admit gives each human or agenda issue without an Agenda number the next number of the
// configured prefix (at least agenda.first), in issue-number order, and sets Status=Todo.
func (d *Daemon) admit(ctx context.Context) error {
	a := d.cfg.Council.Agenda
	next := a.First
	var pending []int
	for n, r := range d.records {
		if k := queue.AgendaNumber(a.Prefix, r.issue.Agenda); k >= next {
			next = k + 1
		}
		if (r.issue.Kind == queue.KindHuman || r.issue.Kind == queue.KindAgenda) && r.issue.Agenda == "" {
			pending = append(pending, n)
		}
	}
	slices.Sort(pending)
	todoName := d.cfg.Project.Fields.Status.Options.Todo
	for _, n := range pending {
		r := d.records[n]
		agenda := fmt.Sprintf("%s%d", a.Prefix, next)
		// Status first: an admission cut short before the Agenda write is retried whole, since
		// only issues without an Agenda number are admitted.
		if err := d.gh.SetOption(ctx, d.project.ID, r.itemID, d.project.Status.ID, d.project.Status.Options[todoName]); err != nil {
			return fmt.Errorf("admit %s: set %s: %w", r.issue.Ref, todoName, err)
		}
		r.issue.Status, r.issue.StatusName = queue.StatusTodo, todoName
		d.records[n] = r
		if err := d.gh.SetText(ctx, d.project.ID, r.itemID, d.project.Agenda, agenda); err != nil {
			return fmt.Errorf("admit %s: set %s: %w", r.issue.Ref, agenda, err)
		}
		r.issue.Agenda = agenda
		d.records[n] = r
		next++
		slog.Info("admitted", "issue", r.issue.Ref, "agenda", agenda, "status", todoName)
	}
	return nil
}
