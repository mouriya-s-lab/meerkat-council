// Package panel convenes and runs one council panel per agenda issue: a draft session for the
// seat with the draft right, then one vote session per voting seat. It keeps no progress of its
// own: before every step it re-reads the issue and derives the next step from the pm:panel,
// pm:draft and pm:vote markers (contract §A). The daemon owns the panel's lifetime through ctx.
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/queue"
	"github.com/oh-my-omp/pm-council/internal/sessions"
)

// GitHub is the part of the GitHub client a panel uses.
type GitHub interface {
	Issue(ctx context.Context, repo string, number int) (github.Issue, error)
	Comment(ctx context.Context, repo string, id int64) (string, error)
	Comments(ctx context.Context, repo string, number int) ([]string, error)
	PostComment(ctx context.Context, repo string, number int, body string) (int64, error)
	AddBlockedBy(ctx context.Context, repo string, number int, blockerID int64) error
	SetOption(ctx context.Context, projectID, itemID, fieldID, optionID string) error
}

// Tally records a seat's vote (internal/tally).
type Tally interface {
	RecordVote(ctx context.Context, ref queue.Ref, seat string, version int, rows []markers.VoteRow) error
}

// ErrInterrupted is the cancel cause the daemon gives a panel it stops for a human issue.
var ErrInterrupted = errors.New("interrupted for a human issue")

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
	Human       bool
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
	StepDraft StepKind = iota // no draft of the current round: run the draft session
	StepVote                  // a draft exists: run the vote sessions of the seats that have not voted on it
	StepDone                  // every voting seat voted on the draft: the issue waits for the tally
)

// Step is the panel's next step derived from the issue's markers.
type Step struct {
	Kind    StepKind
	Round   int
	Version int            // StepDraft: the version to post; otherwise the draft's version
	Draft   *markers.Draft // StepVote, StepDone: the draft voted on
	Pending []string       // StepVote: voting seats without a vote on Version, in seat order
}

// Derive returns the next step: the round is the latest pm:panel's (1 without one); a draft whose
// version is below the round does not belong to it.
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
		return Step{Kind: StepDraft, Round: round, Version: next}
	}
	voted := map[string]bool{}
	for _, m := range ms {
		if v, ok := m.(markers.Vote); ok && v.Version == draft.Version {
			voted[v.Seat] = true
		}
	}
	st := Step{Kind: StepDone, Round: round, Version: draft.Version, Draft: draft}
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

// Run runs the panel until it waits for the tally, is blocked, fails or ctx ends. A failure is
// posted as pm:panel failed; cancellation with cause ErrInterrupted as pm:panel interrupted; any
// other cancellation (the daemon stopping) posts nothing.
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
	pm, post := endMarker(context.Cause(ctx), err, round, open)
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
	case cause != nil:
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
		if st.Kind == StepDone {
			slog.Info("panel: nothing to resume; waiting for the tally", "issue", p.job.Ref, "version", st.Version)
			return st.Round, nil, nil
		}
		if last, ok := markers.LastPanel(is.markers); ok && last.Event == markers.PanelInterrupted && p.cfg.Council.Resume == config.ResumeFork {
			forks = last.Sessions
		}
		resumed := markers.Panel{Event: markers.PanelResumed, Round: st.Round, Sessions: []markers.PanelSession{}}
		if err := p.postPanel(ctx, resumed, fmt.Sprintf("小组恢复审议（第 %d 轮）。", st.Round)); err != nil {
			return st.Round, nil, err
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
			slog.Info("panel: every seat voted; waiting for the tally", "issue", p.job.Ref, "version", st.Version)
			return st.Round, nil, nil
		case StepDraft:
			blocked, open, err := p.draft(ctx, st, is, forks)
			if err != nil || blocked {
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
	bodies, err := p.gh.Comments(ctx, p.repo, p.number)
	if err != nil {
		return issue{}, err
	}
	var errs []error
	is.markers, errs = markers.Collect(bodies)
	for _, err := range errs {
		slog.Error("ignoring malformed marker", "issue", p.job.Ref, "err", err)
	}
	return is, nil
}

// draft runs the draft session and acts on its goal tool. blocked reports that the panel ended
// with pm_block.
func (p *run) draft(ctx context.Context, st Step, is issue, forks []markers.PanelSession) (blocked bool, open []markers.PanelSession, err error) {
	seat := p.cfg.DraftSeat()
	var ps *markers.PanelSession
	res, err := p.session(ctx, config.KindDraft, seat, st, is, fork(forks, config.KindDraft, seat.ID), func(s markers.PanelSession) { ps = &s })
	if err != nil {
		if ps != nil {
			open = []markers.PanelSession{*ps}
		}
		return false, open, err
	}
	// The result is recorded even when an interrupt arrives meanwhile: the session is over.
	w := context.WithoutCancel(ctx)
	switch res.ToolName {
	case config.ToolDraft:
		return false, nil, p.postDraft(w, st, res)
	case config.ToolBlock:
		return true, nil, p.block(w, st, res)
	}
	return false, nil, fmt.Errorf("draft session %s ended with %s, not a draft goal tool", res.SessionID, res.ToolName)
}

type draftArgs struct {
	Rows       []markers.Row   `json:"rows"`
	Supersedes []string        `json:"supersedes"`
	Settle     *markers.Settle `json:"settle"`
	Withdrawn  *string         `json:"withdrawn"`
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
	body, err := markers.Comment(draftText(d), d)
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

type blockArgs struct {
	Prereq string `json:"prereq"`
	Reason string `json:"reason"`
}

// block records the issue as waiting for an undecided human issue: Blocked by, Status=Todo, then
// pm:panel blocked.
func (p *run) block(ctx context.Context, st Step, res sessions.Result) error {
	var a blockArgs
	if err := json.Unmarshal(res.Args, &a); err != nil {
		return fmt.Errorf("%s arguments of session %s: %w", res.ToolName, res.SessionID, err)
	}
	if p.job.Human {
		return fmt.Errorf("session %s blocked human issue %s", res.SessionID, p.job.Ref)
	}
	i := slices.IndexFunc(p.job.HumanIssues, func(h HumanIssue) bool { return strings.EqualFold(string(h.Ref), strings.TrimSpace(a.Prereq)) })
	if i < 0 || p.job.HumanIssues[i].Decided {
		return fmt.Errorf("session %s blocked on %q, not an undecided human issue", res.SessionID, a.Prereq)
	}
	prereq := p.job.HumanIssues[i].Ref
	preRepo, preN, _ := queue.ParseRef(string(prereq))
	pre, err := p.gh.Issue(ctx, preRepo, preN)
	if err != nil {
		return err
	}
	if err := p.gh.AddBlockedBy(ctx, p.repo, p.number, pre.ID); err != nil {
		return err
	}
	todo := p.cfg.Project.Fields.Status.Options.Todo
	if err := p.gh.SetOption(ctx, p.project.ID, p.job.ItemID, p.project.Status.ID, p.project.Status.Options[todo]); err != nil {
		return fmt.Errorf("set %s: %w", todo, err)
	}
	ref := string(prereq)
	pm := markers.Panel{Event: markers.PanelBlocked, Round: st.Round, Sessions: []markers.PanelSession{}, Prereq: &ref}
	text := fmt.Sprintf("小组结束：本议题须等待人类议题 %s 定案后再审议。\n\n%s", prereq, a.Reason)
	if err := p.postPanel(ctx, pm, text); err != nil {
		return err
	}
	slog.Info("panel blocked", "issue", p.job.Ref, "prereq", prereq, "session", res.SessionID)
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
			res, err := p.session(stepCtx, config.KindVote, seat, st, is, fork(forks, config.KindVote, id), func(s markers.PanelSession) {
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
	Human       bool                `json:"human"`
	PMRepo      string              `json:"pmRepo"`
	Repos       contextRepos        `json:"repos"`
	Proxy       config.ProxyReader  `json:"proxy"`
	HumanIssues []HumanIssue        `json:"humanIssues"`
	Draft       *markers.Draft      `json:"draft"`
	Escalation  *markers.Escalation `json:"escalation"`
}

type contextRepos struct {
	Code   []string `json:"code"`
	Design []string `json:"design"`
	OMP    []string `json:"omp"`
}

// session runs one seat session of kind for step st, forked from fork when it is not nil.
func (p *run) session(ctx context.Context, kind config.SessionKind, seat config.Seat, st Step, is issue, fork *markers.PanelSession, onReady func(markers.PanelSession)) (sessions.Result, error) {
	goal := p.cfg.Goals.Of(kind)
	data := config.OpeningData{
		Ref: string(p.job.Ref), Title: is.title, Body: is.body, Agenda: p.job.Agenda,
		Round: st.Round, Version: st.Version, Escalation: is.ask,
	}
	var draft *markers.Draft
	switch kind {
	case config.KindVote:
		draft = st.Draft
		b, err := json.Marshal(draft)
		if err != nil {
			return sessions.Result{}, err
		}
		data.Draft = string(b)
	case config.KindDraft:
	}
	render, forkFile := goal.RenderOpening, ""
	if fork != nil {
		render, forkFile = goal.RenderResume, fork.File
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
		Ref: p.job.Ref, Seat: seat.ID, Rights: seat.Rights, Human: p.job.Human, PMRepo: p.cfg.Repos.PM.Repo,
		Repos: contextRepos{Code: []string{}, Design: p.cfg.Repos.Design, OMP: p.cfg.Repos.OMP},
		Proxy: proxy, HumanIssues: humans, Draft: draft, Escalation: is.escalation,
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
		ContextFile:        contextFile,
		GuardiansURL:       p.cfg.Runtime.GuardiansURL,
		MaxContinuePrompts: p.cfg.Runtime.MaxContinuePrompts,
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

var cell = strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>")

func draftText(d markers.Draft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "决策表第 %d 版。\n\n| id | what | where | how | 取舍 | 分类 | 缺失设计 | 落到 |\n|---|---|---|---|---|---|---|---|\n", d.Version)
	for _, r := range d.Rows {
		deliver := ""
		if r.Deliver != nil {
			deliver = cell.Replace(r.Deliver.Repo + ": " + r.Deliver.Title)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %t | %s |\n",
			cell.Replace(r.ID), cell.Replace(r.What), cell.Replace(r.Where), cell.Replace(r.How),
			cell.Replace(r.Tradeoff), cell.Replace(r.Category), r.MissingDesign, deliver)
	}
	if len(d.Supersedes) > 0 {
		fmt.Fprintf(&b, "\n变更的旧议题：%s\n", strings.Join(d.Supersedes, "、"))
	}
	if d.Settle != nil {
		fmt.Fprintf(&b, "\n询问的了结方式：%s\n\n%s\n", d.Settle.Kind, d.Settle.Text)
	}
	if d.Withdrawn != nil {
		fmt.Fprintf(&b, "\n撤回：%s\n", *d.Withdrawn)
	}
	return b.String()
}
