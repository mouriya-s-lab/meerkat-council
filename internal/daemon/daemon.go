// Package daemon keeps an in-memory copy of the PM repository's agenda issues, rebuilt from
// GitHub at startup, on every forwarded issue event (that one issue) and every rebuild interval
// (everything). It admits new agenda issues (Agenda number, Status=Todo) and publishes the queue.
// GitHub is the only source of state; the copy is always replaceable by a rebuild.
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
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// GitHub is the part of the GitHub client the daemon uses.
type GitHub interface {
	ProjectItems(ctx context.Context, projectID string) ([]github.Item, error)
	IssueItem(ctx context.Context, repo string, number int, projectID string) (github.Item, bool, error)
	Comments(ctx context.Context, repo string, number int) ([]string, error)
	BlockedBy(ctx context.Context, repo string, number int) ([]github.IssueRef, error)
	SetText(ctx context.Context, projectID, itemID, fieldID, text string) error
	SetOption(ctx context.Context, projectID, itemID, fieldID, optionID string) error
}

// record is one PM-repository issue in the Project as last read from GitHub.
type record struct {
	itemID string
	issue  queue.Issue
}

type Daemon struct {
	cfg     *config.Config
	gh      GitHub
	project github.Project
	events  chan int
	queue   atomic.Pointer[queue.Queue]

	// owned by the worker goroutine (Run) after New's initial rebuild
	records map[int]record
}

// New performs the startup full rebuild (with admission) and returns the daemon ready to Run.
func New(ctx context.Context, cfg *config.Config, gh GitHub, project github.Project) (*Daemon, error) {
	d := &Daemon{cfg: cfg, gh: gh, project: project, events: make(chan int, 64), records: map[int]record{}}
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

// Run serves events and periodic full rebuilds until ctx ends.
func (d *Daemon) Run(ctx context.Context) {
	tick := time.NewTicker(d.cfg.Runtime.Daemon.RebuildInterval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := d.rebuild(ctx); err != nil {
				slog.Error("full rebuild failed; keeping the previous copy", "err", err)
				continue
			}
			slog.Info("full rebuild", "issues", len(d.records))
		case n := <-d.events:
			if err := d.refresh(ctx, n); err != nil {
				slog.Error("issue refresh failed", "issue", n, "err", err)
				continue
			}
		}
		d.admitAndPublish(ctx)
	}
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
	bodies, err := d.gh.Comments(ctx, it.Repo, it.Number)
	if err != nil {
		return record{}, err
	}
	for _, b := range bodies {
		m, err := markers.Parse(b)
		switch {
		case errors.Is(err, markers.ErrNoMarker):
		case err != nil:
			slog.Error("ignoring malformed marker", "issue", ref, "err", err)
		default:
			is.Markers = append(is.Markers, m)
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
