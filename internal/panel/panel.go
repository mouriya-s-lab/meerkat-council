// Package panel convenes and runs one council panel per agenda issue: a draft session for the
// seat with the draft right, one vote session per voting seat, then the tally: an arbitrate
// session for category disputes, a new round with a revise session for counterexamples, or the
// decision's publication. It keeps no progress of its own: before every step it re-reads the
// issue and derives the next step from the pm:panel, pm:draft, pm:vote, pm:ruling and
// pm:decision markers (contract §A). The daemon owns the panel's lifetime through ctx.
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mouriya-s-lab/meerkat-council/internal/config"
	"github.com/mouriya-s-lab/meerkat-council/internal/github"
	"github.com/mouriya-s-lab/meerkat-council/internal/markers"
	"github.com/mouriya-s-lab/meerkat-council/internal/queue"
	"github.com/mouriya-s-lab/meerkat-council/internal/sessions"
	"github.com/mouriya-s-lab/meerkat-council/internal/tally"
)

// GitHub is the part of the GitHub client a panel uses.
type GitHub interface {
	Issue(ctx context.Context, repo string, number int) (github.Issue, error)
	Comment(ctx context.Context, repo string, id int64) (string, error)
	EachComment(ctx context.Context, repo string, number int, fn func(body string, createdAt time.Time) error) error
	PostComment(ctx context.Context, repo string, number int, body string) (int64, error)
	SetOption(ctx context.Context, projectID, itemID, fieldID, optionID string) error
}

// Tally records votes and rulings and publishes decisions (internal/tally).
type Tally interface {
	RecordVote(ctx context.Context, ref queue.Ref, seat string, version int, rows []markers.VoteRow) error
	RecordRuling(ctx context.Context, ref queue.Ref, seat string, version int, rows []markers.RulingRow) error
	Publish(ctx context.Context, p tally.Publication) error
}

// ErrInterrupted is the cancel cause the daemon gives a panel it stops for a human issue.
var ErrInterrupted = errors.New("interrupted for a human issue")

// ErrLeftDeliberation is the cancel cause the daemon gives a panel whose issue was closed or set
// to done or superseded before it carried a pm:decision. The panel is not resumed, so it posts
// nothing.
var ErrLeftDeliberation = errors.New("the issue left deliberation")

// HumanIssue is a human issue of the snapshot the panel was started from.
type HumanIssue struct {
	Ref     queue.Ref `json:"ref"`
	Decided bool      `json:"decided"`
}

// Job is one panel to run.
type Job struct {
	Ref         queue.Ref
	ItemID      string // Project item id
	Agenda      string // Agenda field
	Start       queue.Start
	HumanIssues []HumanIssue
}

// Runner runs panels. Its methods may be called from several goroutines.
type Runner struct {
	cfg      *config.Config
	gh       GitHub
	project  github.Project
	sessions *sessions.Registry
	tally    Tally

	checkoutMu sync.Mutex
}

func New(cfg *config.Config, gh GitHub, project github.Project, reg *sessions.Registry, t Tally) *Runner {
	return &Runner{cfg: cfg, gh: gh, project: project, sessions: reg, tally: t}
}

// StepKind is the next thing a panel does.
type StepKind int

const (
	StepDraft StepKind = iota // no draft of the current round: run the draft (round 1) or revise session
	StepVote                  // a draft exists: run the vote sessions of the seats that have not voted on it
	StepDone                  // every voting seat voted on the draft, or it is withdrawn: tally it
)

// Step is the panel's next step derived from the issue's markers.
type Step struct {
	Kind    StepKind
	Round   int
	Version int // StepDraft: the version to post; otherwise the draft's version
	// Draft is the draft voted on (StepVote, StepDone) or revised (StepDraft after round 1); nil
	// for the first draft.
	Draft   *markers.Draft
	Pending []string // StepVote: voting seats without a vote on Version, in seat order
}

// Derive returns the next step: the round is the latest pm:panel's (1 without one); a draft whose
// version is below the round does not belong to it. A withdrawn draft is not voted on.
func Derive(ms []markers.Marker, voters []string) Step {
	round := 1
	if p, ok := markers.LastPanel(ms); ok && p.Round > 0 {
		round = p.Round
	}
	var draft *markers.Draft
	for _, m := range ms {
		if d, ok := m.(markers.Draft); ok && (draft == nil || d.Version > draft.Version) {
			draft = &d
		}
	}
	if draft == nil || draft.Version < round {
		next := 1
		if draft != nil {
			next = draft.Version + 1
		}
		return Step{Kind: StepDraft, Round: round, Version: next, Draft: draft}
	}
	st := Step{Kind: StepDone, Round: round, Version: draft.Version, Draft: draft}
	if draft.Withdrawn != nil {
		return st
	}
	voted := map[string]bool{}
	for _, m := range ms {
		if v, ok := m.(markers.Vote); ok && v.Version == draft.Version {
			voted[v.Seat] = true
		}
	}
	for _, s := range voters {
		if !voted[s] {
			st.Pending = append(st.Pending, s)
		}
	}
	if len(st.Pending) > 0 {
		st.Kind = StepVote
	}
	return st
}

// Run runs the panel until its decision is published, it fails or ctx ends. A failure is posted
// as pm:panel failed; cancellation with cause ErrInterrupted as pm:panel interrupted;
// cancellation with cause ErrLeftDeliberation only logs the killed sessions; a daemon stop
// (cause sessions.ErrShutdown) posts nothing. The sessions it kills are recorded by sessions.Registry.
func (r *Runner) Run(ctx context.Context, j Job) {
	repo, n, ok := queue.ParseRef(string(j.Ref))
	if !ok {
		slog.Error("panel: bad issue ref", "issue", j.Ref)
		return
	}
	p := &run{Runner: r, job: j, repo: repo, number: n}
	round, open, err := p.run(ctx)
	if err == nil {
		return
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrLeftDeliberation) {
		slog.Info("panel stopped: the issue left deliberation", "issue", j.Ref, "killed", open)
		return
	}
	pm, post := endMarker(cause, err, round, open)
	if !post {
		slog.Info("panel stopped with the daemon", "issue", j.Ref, "err", err)
		return
	}
	var text string
	switch pm.Event {
	case markers.PanelInterrupted:
		slog.Info("panel interrupted", "issue", j.Ref, "killed", open)
		text = fmt.Sprintf("小组因人类议题被中止（第 %d 轮）；被中止的会话列在标记里，定案后按配置恢复。", round)
	case markers.PanelFailed:
		slog.Error("panel failed", "issue", j.Ref, "err", err, "running", open)
		text = fmt.Sprintf("小组失败（第 %d 轮）：\n\n```text\n%s\n```", round, *pm.Error)
	case markers.PanelStarted, markers.PanelResumed, markers.PanelBlocked:
		panic(fmt.Sprintf("panel end marker %q", pm.Event))
	}
	if err := p.postPanel(context.WithoutCancel(ctx), pm, text); err != nil {
		slog.Error("panel: post failed", "issue", j.Ref, "event", pm.Event, "err", err)
	}
}

// endMarker is the pm:panel a run that ended with err posts, given ctx's cancel cause: interrupted
// with the killed sessions, or failed with the error and no sessions (contract §A: sessions only
// for interrupted). post is false when the daemon stopped the panel.
func endMarker(cause, err error, round int, open []markers.PanelSession) (pm markers.Panel, post bool) {
	switch {
	case errors.Is(cause, ErrInterrupted):
		if open == nil {
			open = []markers.PanelSession{}
		}
		return markers.Panel{Event: markers.PanelInterrupted, Round: round, Sessions: open}, true
	case errors.Is(cause, sessions.ErrShutdown):
		return markers.Panel{}, false
	}
	reason := err.Error()
	return markers.Panel{Event: markers.PanelFailed, Round: round, Sessions: []markers.PanelSession{}, Error: &reason}, true
}

// run is one panel run of one issue.
type run struct {
	*Runner
	job    Job
	repo   string
	number int
}

// issue is the issue as read before a step.
type issue struct {
	title, body string
	escalation  *markers.Escalation
	ask         *config.EscalationData
	markers     []markers.Marker
}

// run returns the round reached and, with an error, the sessions that were running when it ended.
func (p *run) run(ctx context.Context) (int, []markers.PanelSession, error) {
	is, err := p.read(ctx)
	if err != nil {
		return 1, nil, err
	}
	st := Derive(is.markers, p.voters())
	var forks []markers.PanelSession
	switch p.job.Start {
	case queue.StartConvene:
		st.Round = 1
		inProgress := p.cfg.Project.Fields.Status.Options.InProgress
		if err := p.gh.SetOption(ctx, p.project.ID, p.job.ItemID, p.project.Status.ID, p.project.Status.Options[inProgress]); err != nil {
			return st.Round, nil, fmt.Errorf("set %s: %w", inProgress, err)
		}
		started := markers.Panel{Event: markers.PanelStarted, Round: 1, Sessions: []markers.PanelSession{}}
		if err := p.postPanel(ctx, started, "小组开始审议（第 1 轮）。"); err != nil {
			return st.Round, nil, err
		}
	case queue.StartResume:
		last, ok := markers.LastPanel(is.markers)
		if ok && last.Event == markers.PanelInterrupted && p.cfg.Council.Resume == config.ResumeFork {
			forks = last.Sessions
		}
		// An issue whose sessions are all over and whose panel did not stop waits only for its
		// tally; it is not resumed, or every rebuild would announce it again.
		if st.Kind != StepDone || ok && last.Event != markers.PanelStarted && last.Event != markers.PanelResumed {
			resumed := markers.Panel{Event: markers.PanelResumed, Round: st.Round, Sessions: []markers.PanelSession{}}
			if err := p.postPanel(ctx, resumed, fmt.Sprintf("小组恢复审议（第 %d 轮）。", st.Round)); err != nil {
				return st.Round, nil, err
			}
		}
	}
	if err := p.syncCheckout(ctx); err != nil {
		return st.Round, nil, err
	}
	for {
		if is, err = p.read(ctx); err != nil {
			return st.Round, nil, err
		}
		st = Derive(is.markers, p.voters())
		switch st.Kind {
		case StepDone:
			decided, open, err := p.decide(ctx, st, is, forks)
			if err != nil || decided {
				return st.Round, open, err
			}
		case StepDraft:
			if open, err := p.draft(ctx, st, is, forks); err != nil {
				return st.Round, open, err
			}
		case StepVote:
			if open, err := p.vote(ctx, st, is, forks); err != nil {
				return st.Round, open, err
			}
		}
		// A killed session is forked only by the first step after the resume.
		forks = nil
	}
}

// decide tallies the voted draft. Arbitrate runs the arbitrate session and records its ruling;
// Revise opens the next round; Decide publishes the decision and ends the run. A decision already
// posted for the version is published again, which finishes whatever its publish left undone.
func (p *run) decide(ctx context.Context, st Step, is issue, forks []markers.PanelSession) (decided bool, open []markers.PanelSession, err error) {
	var (
		votes   []markers.Vote
		rulings []markers.Ruling
		posted  *markers.Decision
	)
	for _, m := range is.markers {
		switch m := m.(type) {
		case markers.Vote:
			votes = append(votes, m)
		case markers.Ruling:
			rulings = append(rulings, m)
		case markers.Decision:
			if m.Version == st.Version {
				posted = &m
			}
		}
	}
	if posted != nil {
		return true, nil, p.publish(ctx, st, posted.Rows, true)
	}
	var humans []string
	for _, h := range p.job.HumanIssues {
		humans = append(humans, string(h.Ref))
	}
	switch o := tally.Evaluate(*st.Draft, votes, rulings, p.cfg.Seats, p.cfg.Tally, humans, st.Round).(type) {
	case tally.Arbitrate:
		slog.Info("tally: category dispute", "issue", p.job.Ref, "version", st.Version, "rows", len(o.Disputes))
		open, err := p.arbitrate(ctx, st, is, o.Disputes, forks)
		return false, open, err
	case tally.Revise:
		round := st.Round + 1
		slog.Info("tally: counterexamples; next round", "issue", p.job.Ref, "version", st.Version, "rows", o.Rows, "round", round)
		started := markers.Panel{Event: markers.PanelStarted, Round: round, Sessions: []markers.PanelSession{}}
		return false, nil, p.postPanel(ctx, started, fmt.Sprintf("小组开始第 %d 轮审议：修订决策表第 %d 版的反例（%s）。", round, st.Version, strings.Join(o.Rows, "、")))
	case tally.Decide:
		return true, nil, p.publish(ctx, st, o.Rows, false)
	default:
		panic(fmt.Sprintf("unknown tally outcome %T", o))
	}
}

// publish publishes the decision of st's draft. An interrupt stops the panel only before the
// first publish starts; once it has started, or when the decision is already posted (the issue
// is decided and its publish is being finished), the publish runs to its end, so a human panel
// never starts beside a half-published decision. The daemon stopping stops it either way.
func (p *run) publish(ctx context.Context, st Step, rows []markers.DecisionRow, posted bool) error {
	if ctx.Err() != nil && (!posted || !errors.Is(context.Cause(ctx), ErrInterrupted)) {
		return context.Cause(ctx)
	}
	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() {
		if !errors.Is(context.Cause(ctx), ErrInterrupted) {
			cancel()
		}
	})
	defer func() {
		stop()
		cancel()
	}()
	return p.tally.Publish(pctx, tally.Publication{Ref: p.job.Ref, Agenda: p.job.Agenda, Draft: *st.Draft, Rows: rows})
}

// arbitrate runs the arbitration seat's session on the disputed rows and records its pm_ruling.
func (p *run) arbitrate(ctx context.Context, st Step, is issue, disputes []tally.Dispute, forks []markers.PanelSession) ([]markers.PanelSession, error) {
	seat, _ := p.cfg.Seat(p.cfg.Tally.Arbitration.Seat)
	var ps *markers.PanelSession
	res, err := p.session(ctx, config.KindArbitrate, seat, st, is, fork(forks, config.KindArbitrate, seat.ID), disputes, func(s markers.PanelSession) { ps = &s })
	if err != nil {
		var open []markers.PanelSession
		if ps != nil {
			open = []markers.PanelSession{*ps}
		}
		return open, err
	}
	if res.ToolName != config.ToolRuling {
		return nil, fmt.Errorf("arbitrate session %s ended with %s, not %s", res.SessionID, res.ToolName, config.ToolRuling)
	}
	var a struct {
		Rows []markers.RulingRow `json:"rows"`
	}
	if err := json.Unmarshal(res.Args, &a); err != nil {
		return nil, fmt.Errorf("%s arguments of session %s: %w", res.ToolName, res.SessionID, err)
	}
	if err := rulingCovers(a.Rows, disputes, p.cfg.Tally.Categories); err != nil {
		return nil, fmt.Errorf("%s of session %s: %w", res.ToolName, res.SessionID, err)
	}
	// The result is recorded even when an interrupt arrives meanwhile: the session is over.
	return nil, p.tally.RecordRuling(context.WithoutCancel(ctx), p.job.Ref, seat.ID, st.Version, a.Rows)
}

// rulingCovers checks that rows rule each disputed row exactly once with one of categories (the
// plugin checks the same; the ruling decides the tally, so the program does not take it on trust).
func rulingCovers(rows []markers.RulingRow, disputes []tally.Dispute, categories []string) error {
	want := map[string]bool{}
	for _, d := range disputes {
		want[d.Row.ID] = true
	}
	seen := map[string]bool{}
	for _, r := range rows {
		switch {
		case !want[r.Row]:
			return fmt.Errorf("row %s is not disputed", r.Row)
		case seen[r.Row]:
			return fmt.Errorf("row %s is ruled twice", r.Row)
		case !slices.Contains(categories, r.Category):
			return fmt.Errorf("row %s: category %q is not one of %s", r.Row, r.Category, strings.Join(categories, ", "))
		}
		seen[r.Row] = true
	}
	if len(seen) != len(want) {
		return fmt.Errorf("rules %d of %d disputed rows", len(seen), len(want))
	}
	return nil
}

func (p *run) voters() []string {
	var ids []string
	for _, s := range p.cfg.Seats {
		if s.Has(config.RightVote) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// read re-reads the issue: title, body, the escalation marker of its body with the ask's text,
// and its comment markers.
func (p *run) read(ctx context.Context) (issue, error) {
	gi, err := p.gh.Issue(ctx, p.repo, p.number)
	if err != nil {
		return issue{}, err
	}
	is := issue{title: gi.Title, body: gi.Body}
	if m, err := markers.Parse(gi.Body); err == nil {
		if e, ok := m.(markers.Escalation); ok {
			repo, _, ok := queue.ParseRef(e.Issue)
			if !ok {
				return issue{}, fmt.Errorf("escalation issue %q is not owner/repo#n", e.Issue)
			}
			text, err := p.gh.Comment(ctx, repo, e.Ask)
			if err != nil {
				return issue{}, err
			}
			is.escalation = &e
			is.ask = &config.EscalationData{Issue: e.Issue, Ask: e.Ask, AskText: text}
		}
	}
	// Streams page by page without retaining bodies; malformed markers are logged.
	if err := p.gh.EachComment(ctx, p.repo, p.number, func(body string, _ time.Time) error {
		m, err := markers.Parse(body)
		switch {
		case errors.Is(err, markers.ErrNoMarker):
		case err != nil:
			slog.Error("ignoring malformed marker", "issue", p.job.Ref, "err", err)
		default:
			is.markers = append(is.markers, m)
		}
		return nil
	}); err != nil {
		return issue{}, err
	}
	return is, nil
}

// draft runs the draft session, or in a later round the revise session, and posts its pm_draft.
func (p *run) draft(ctx context.Context, st Step, is issue, forks []markers.PanelSession) ([]markers.PanelSession, error) {
	seat, kind := p.cfg.DraftSeat(), config.KindDraft
	if st.Draft != nil {
		kind = config.KindRevise
	}
	var ps *markers.PanelSession
	res, err := p.session(ctx, kind, seat, st, is, fork(forks, kind, seat.ID), nil, func(s markers.PanelSession) { ps = &s })
	if err != nil {
		var open []markers.PanelSession
		if ps != nil {
			open = []markers.PanelSession{*ps}
		}
		return open, err
	}
	if res.ToolName != config.ToolDraft {
		return nil, fmt.Errorf("draft session %s ended with %s, not %s", res.SessionID, res.ToolName, config.ToolDraft)
	}
	// The result is recorded even when an interrupt arrives meanwhile: the session is over.
	return nil, p.postDraft(context.WithoutCancel(ctx), st, res)
}

type draftArgs struct {
	Rows       []markers.Row   `json:"rows"`
	Supersedes []string        `json:"supersedes"`
	Settle     *markers.Settle `json:"settle"`
	Withdrawn  *string         `json:"withdrawn"`
}

// normalizeDeliver makes every row of args carry a present deliver: a row without a deliver gets a
// explicit null (the marker shape's "no implementation issue"), a row with a null deliver keeps it,
// and a row with an object keeps the object. It returns the decoded rows; args is only read.
func normalizeDeliver(args json.RawMessage) ([]markers.Row, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return nil, err
	}
	var rawRows []json.RawMessage
	if incoming, ok := obj["rows"]; !ok || incoming == nil {
		rawRows = []json.RawMessage{}
	} else if err := json.Unmarshal(incoming, &rawRows); err != nil {
		return nil, err
	}
	rows := make([]markers.Row, 0, len(rawRows))
	for i, raw := range rawRows {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("rows[%d]: %w", i, err)
		}
		if _, ok := row["deliver"]; !ok {
			row["deliver"] = json.RawMessage("null")
		}
		var r markers.Row
		if fixed, err := json.Marshal(row); err != nil {
			return nil, fmt.Errorf("rows[%d]: %w", i, err)
		} else if err = json.Unmarshal(fixed, &r); err != nil {
			return nil, fmt.Errorf("rows[%d]: %w", i, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func (p *run) postDraft(ctx context.Context, st Step, res sessions.Result) error {
	var a draftArgs
	if err := json.Unmarshal(res.Args, &a); err != nil {
		return fmt.Errorf("%s arguments of session %s: %w", res.ToolName, res.SessionID, err)
	}
	if a.Rows == nil {
		a.Rows = []markers.Row{}
	}
	if a.Supersedes == nil {
		a.Supersedes = []string{}
	}
	// An omitted withdrawn or settle decodes as nil, the same as null: no withdrawal, no settle.
	if a.Withdrawn != nil && st.Draft == nil {
		return fmt.Errorf("session %s: withdrawn must be null: only a revise session may withdraw the agenda", res.SessionID)
	}
	// pm_draft's schema makes deliver optional; an omitted deliver is no delivery, and the marker
	// shape (contract §A) is a deliver object or "deliver":null — a nil Row across json.Marshal
	// writes no deliver key, which markers.Parse would refuse. Re-decode with normalization.
	rows, err := normalizeDeliver(res.Args)
	if err != nil {
		return fmt.Errorf("%s arguments of session %s: %w", res.ToolName, res.SessionID, err)
	}
	a.Rows = rows
	is, err := p.read(ctx)
	if err != nil {
		return err
	}
	highest := 0
	for _, m := range is.markers {
		if d, ok := m.(markers.Draft); ok && d.Version > highest {
			highest = d.Version
		}
	}
	if highest >= st.Round {
		slog.Info("draft dropped: the round already has one", "issue", p.job.Ref, "session", res.SessionID, "version", highest)
		return nil
	}
	d := markers.Draft{Version: highest + 1, Rows: a.Rows, Supersedes: a.Supersedes, Settle: a.Settle, Withdrawn: a.Withdrawn}
	body, err := markers.Comment(tally.DraftText(d), d)
	if err != nil {
		return fmt.Errorf("draft of session %s: %w", res.SessionID, err)
	}
	id, err := p.gh.PostComment(ctx, p.repo, p.number, body)
	if err != nil {
		return err
	}
	slog.Info("draft posted", "issue", p.job.Ref, "version", d.Version, "comment", id, "session", res.SessionID)
	return nil
}

// vote runs the vote sessions of the pending seats concurrently and records each vote as it
// arrives. The first failure kills the others.
func (p *run) vote(ctx context.Context, st Step, is issue, forks []markers.PanelSession) ([]markers.PanelSession, error) {
	stepCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	type slot struct {
		ps   *markers.PanelSession
		done bool
	}
	var (
		mu    sync.Mutex
		slots = make([]slot, len(st.Pending))
		first error
		wg    sync.WaitGroup
	)
	for i, id := range st.Pending {
		seat, _ := p.cfg.Seat(id)
		wg.Go(func() {
			res, err := p.session(stepCtx, config.KindVote, seat, st, is, fork(forks, config.KindVote, id), nil, func(s markers.PanelSession) {
				mu.Lock()
				slots[i].ps = &s
				mu.Unlock()
			})
			if err == nil {
				err = p.recordVote(context.WithoutCancel(ctx), st, seat.ID, res)
			}
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				slots[i].done = true
				return
			}
			if first == nil && stepCtx.Err() == nil {
				first = err
				cancel(err)
			}
		})
	}
	wg.Wait()
	var open []markers.PanelSession
	for _, s := range slots {
		if s.ps != nil && !s.done {
			open = append(open, *s.ps)
		}
	}
	if ctx.Err() != nil {
		return open, context.Cause(ctx)
	}
	return open, first
}

func (p *run) recordVote(ctx context.Context, st Step, seat string, res sessions.Result) error {
	if res.ToolName != config.ToolVote {
		return fmt.Errorf("vote session %s ended with %s, not %s", res.SessionID, res.ToolName, config.ToolVote)
	}
	var a struct {
		Rows []markers.VoteRow `json:"rows"`
	}
	if err := json.Unmarshal(res.Args, &a); err != nil {
		return fmt.Errorf("%s arguments of session %s: %w", res.ToolName, res.SessionID, err)
	}
	if a.Rows == nil {
		a.Rows = []markers.VoteRow{}
	}
	return p.tally.RecordVote(ctx, p.job.Ref, seat, st.Version, a.Rows)
}

// forkFile returns the session file to fork from for kind and seat, or "" to start a fresh
// session. A fork entry whose file no longer exists (the killed session never wrote it before
// it was interrupted) starts fresh: there is no context to inherit, so its path must not
// reach --fork. Only a missing file goes fresh; any other stat error keeps the fork so the
// failure stays visible. The missing-file case logs one line here; callers log nothing for "".
func (p *run) forkFile(kind config.SessionKind, seat string, fork *markers.PanelSession) string {
	if fork == nil || fork.File == "" {
		return ""
	}
	if _, err := os.Stat(fork.File); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fork.File
		}
		slog.Warn("session fork source missing, starting a fresh session",
			"issue", p.job.Ref, "kind", kind, "seat", seat, "fork", fork.File, "err", err)
		return ""
	}
	return fork.File
}

// fork returns the killed session of kind and seat to fork, or nil.
func fork(forks []markers.PanelSession, kind config.SessionKind, seat string) *markers.PanelSession {
	i := slices.IndexFunc(forks, func(s markers.PanelSession) bool { return s.Kind == string(kind) && s.Seat == seat })
	if i < 0 {
		return nil
	}
	return &forks[i]
}

// sessionContext is the PM_CONTEXT file of a session.
type sessionContext struct {
	Ref         queue.Ref           `json:"ref"`
	Seat        string              `json:"seat"`
	Rights      []config.Right      `json:"rights"`
	PMRepo      string              `json:"pmRepo"`
	Repos       contextRepos        `json:"repos"`
	Proxy       config.ProxyReader  `json:"proxy"`
	HumanIssues []HumanIssue        `json:"humanIssues"`
	Draft       *markers.Draft      `json:"draft"`
	Escalation  *markers.Escalation `json:"escalation"`
	Categories  []string            `json:"categories"`
	Disputed    []string            `json:"disputed"`
}

type contextRepos struct {
	Code   []string `json:"code"`
	Design []string `json:"design"`
	OMP    []string `json:"omp"`
}

// session runs one seat session of kind for step st, forked from fork when it is not nil.
// disputes are the rows an arbitrate session rules on.
func (p *run) session(ctx context.Context, kind config.SessionKind, seat config.Seat, st Step, is issue, fork *markers.PanelSession, disputes []tally.Dispute, onReady func(markers.PanelSession)) (sessions.Result, error) {
	goal := p.cfg.Goals.Of(kind)
	data := config.OpeningData{
		Ref: string(p.job.Ref), Title: is.title, Body: is.body, Agenda: p.job.Agenda,
		Round: st.Round, Version: st.Version, MaxRounds: p.cfg.Tally.MaxRounds, Escalation: is.ask,
		Categories: p.cfg.Tally.Categories,
	}
	disputed := []string{}
	switch kind {
	case config.KindDraft:
	case config.KindVote:
	case config.KindRevise:
		var votes []markers.Vote
		for _, m := range is.markers {
			if v, ok := m.(markers.Vote); ok {
				votes = append(votes, v)
			}
		}
		for _, c := range tally.Counterexamples(*st.Draft, votes, p.cfg.Seats) {
			data.Counterexamples = append(data.Counterexamples, config.CounterexampleData{Seat: c.Seat, Row: c.Row, Counterexample: c.Text})
		}
	case config.KindArbitrate:
		for _, d := range disputes {
			dd := config.DisputeData{Row: d.Row, Seat: d.Seat}
			if d.Citation != nil {
				dd.Citation = *d.Citation
			}
			if d.Classification != nil {
				dd.Classification = *d.Classification
			}
			data.Disputes = append(data.Disputes, dd)
			disputed = append(disputed, d.Row.ID)
		}
	default:
		panic(fmt.Sprintf("unknown session kind %q", kind))
	}
	if st.Draft != nil {
		b, err := json.Marshal(st.Draft)
		if err != nil {
			return sessions.Result{}, err
		}
		data.Draft = string(b)
	}
	render, forkFile := goal.RenderOpening, ""
	if fork != nil {
		if forkFile = p.forkFile(kind, seat.ID, fork); forkFile != "" {
			render = goal.RenderResume
		}
	}
	opening, err := render(data)
	if err != nil {
		return sessions.Result{}, fmt.Errorf("%s template: %w", kind, err)
	}

	dir := filepath.Join(p.cfg.Runtime.SessionsDir, strconv.Itoa(p.number))
	contextFile := filepath.Join(dir, "context", fmt.Sprintf("%s-%s-r%d-v%d.json", kind, seat.ID, st.Round, st.Version))
	proxy, _ := p.cfg.ProxyReader(seat.ID)
	humans := p.job.HumanIssues
	if humans == nil {
		humans = []HumanIssue{}
	}
	sc := sessionContext{
		Ref: p.job.Ref, Seat: seat.ID, Rights: seat.Rights, PMRepo: p.cfg.Repos.PM.Repo,
		Repos: contextRepos{Code: []string{}, Design: p.cfg.Repos.Design, OMP: p.cfg.Repos.OMP},
		Proxy: proxy, HumanIssues: humans, Draft: st.Draft, Escalation: is.escalation,
		Categories: p.cfg.Tally.Categories, Disputed: disputed,
	}
	for _, c := range p.cfg.Repos.Code {
		sc.Repos.Code = append(sc.Repos.Code, c.Repo)
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return sessions.Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(contextFile), 0o755); err != nil {
		return sessions.Result{}, err
	}
	if err := os.WriteFile(contextFile, b, 0o644); err != nil {
		return sessions.Result{}, err
	}

	slog.Info("session starting", "issue", p.job.Ref, "kind", kind, "seat", seat.ID, "round", st.Round, "version", st.Version, "fork", forkFile)
	return p.sessions.Run(ctx, sessions.Spec{
		OMP:          p.cfg.Runtime.OMP,
		Model:        seat.Model,
		SystemPrompt: seat.Prompt,
		Tools:        strings.Join(slices.Concat(seat.Tools, kind.PluginTools()), ","),
		SessionDir:   dir,
		Cwd:          p.checkoutDir(),
		ForkFrom:     forkFile,
		GoalTools:    goal.GoalTools,
		Opening:      opening,
		Continue:     goal.Continue,
		Meta: sessions.Meta{
			Kind: kind, Seat: seat.ID, Model: seat.Model, Ref: string(p.job.Ref), Round: st.Round, Version: st.Version,
		},
		ContextFile:           contextFile,
		GuardiansURL:          p.cfg.Runtime.GuardiansURL,
		MaxContinuePrompts:    p.cfg.Runtime.MaxContinuePrompts,
		SessionTimeoutSeconds: p.cfg.Runtime.SessionTimeoutSeconds,
		OnReady: func(id, file string) {
			slog.Info("session ready", "issue", p.job.Ref, "kind", kind, "seat", seat.ID, "session", id, "file", file)
			onReady(markers.PanelSession{Seat: seat.ID, Kind: string(kind), Session: id, File: file})
		},
	})
}

func (p *run) postPanel(ctx context.Context, pm markers.Panel, text string) error {
	body, err := markers.Comment(text, pm)
	if err != nil {
		return err
	}
	id, err := p.gh.PostComment(ctx, p.repo, p.number, body)
	if err != nil {
		return err
	}
	slog.Info("panel posted", "issue", p.job.Ref, "event", pm.Event, "round", pm.Round, "comment", id)
	return nil
}

func (r *Runner) checkoutDir() string {
	return filepath.Join(r.cfg.Runtime.CheckoutDir, filepath.FromSlash(r.cfg.Repos.PM.Repo))
}

// syncCheckout clones the PM repository on first use, then fetches and resets it to the remote's
// default branch. Git authenticates with the host's credential helper.
func (r *Runner) syncCheckout(ctx context.Context) error {
	r.checkoutMu.Lock()
	defer r.checkoutMu.Unlock()
	dir := r.checkoutDir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		if err := git(ctx, "", "clone", "https://github.com/"+r.cfg.Repos.PM.Repo+".git", dir); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := git(ctx, dir, "fetch", "origin"); err != nil {
		return err
	}
	return git(ctx, dir, "reset", "--hard", "origin/HEAD")
}

func git(ctx context.Context, dir string, args ...string) error {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
