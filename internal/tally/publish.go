package tally

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// Publication is a decided draft version of an agenda issue: the draft and Evaluate's rows.
type Publication struct {
	Ref    queue.Ref
	Agenda string // the Agenda field
	Draft  markers.Draft
	Rows   []markers.DecisionRow
}

// Publish carries the decision of p out on GitHub, in order: implementation issues for passed rows
// with deliver; pm:decision; the superseded issues of a human issue; the escalated ask's
// settlement; the archive commit; the agenda issue closed and Status=Done. Every step first reads
// GitHub and skips what is already there, so a publish cut short anywhere is finished by running
// it again. Once a pm:decision of the version exists, its impl and superseded lists are the
// decision, and p.Rows is not used.
func (t *Tally) Publish(ctx context.Context, p Publication) error {
	repo, n, ok := queue.ParseRef(string(p.Ref))
	if !ok {
		return fmt.Errorf("publish: bad issue ref %q", p.Ref)
	}
	pb := &publish{Tally: t, p: p, repo: repo, number: n}
	return pb.run(ctx)
}

type publish struct {
	*Tally
	p      Publication
	repo   string
	number int
	agenda github.Issue
	esc    *markers.Escalation
}

func (pb *publish) run(ctx context.Context) error {
	var err error
	if pb.agenda, err = pb.gh.Issue(ctx, pb.repo, pb.number); err != nil {
		return err
	}
	if m, err := markers.Parse(pb.agenda.Body); err == nil {
		if e, ok := m.(markers.Escalation); ok {
			pb.esc = &e
		}
	}
	_, _, ms, err := pb.markers(ctx, pb.p.Ref)
	if err != nil {
		return err
	}
	dec, found := decisionOf(ms, pb.p.Draft.Version)
	rows := pb.p.Rows
	if found {
		rows = dec.Rows
	}
	deliver := passedDeliverRows(pb.p.Draft, rows)
	for _, r := range deliver {
		if err := checkDeliver(r, pb.cfg.Repos.Code); err != nil {
			return err
		}
	}
	replacement := pb.replacement(deliver)

	if !found {
		impl, err := pb.implIssues(ctx, deliver, replacement)
		if err != nil {
			return err
		}
		superseded, refusals, err := pb.supersedable(ctx)
		if err != nil {
			return err
		}
		dec = markers.Decision{Version: pb.p.Draft.Version, Rows: withRefusals(rows, refusals), Impl: impl, Superseded: superseded}
		id, err := pb.post(ctx, pb.repo, pb.number, decisionText(dec, refusals), dec)
		if err != nil {
			return err
		}
		slog.Info("decision posted", "issue", pb.p.Ref, "version", dec.Version, "impl", dec.Impl, "superseded", dec.Superseded, "comment", id)
	}
	if err := pb.supersede(ctx, dec.Superseded); err != nil {
		return err
	}
	if err := pb.settle(ctx, dec, replacement); err != nil {
		return err
	}
	if err := pb.archive(ctx, dec, ms); err != nil {
		return err
	}
	return pb.finish(ctx)
}

// decisionOf returns the latest pm:decision of version.
func decisionOf(ms []markers.Marker, version int) (markers.Decision, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if d, ok := ms[i].(markers.Decision); ok && d.Version == version {
			return d, true
		}
	}
	return markers.Decision{}, false
}

// passedDeliverRows are the draft rows that passed and have a deliver, in draft order. The
// decision's impl list follows this order.
func passedDeliverRows(d markers.Draft, rows []markers.DecisionRow) []markers.Row {
	passed := map[string]bool{}
	for _, r := range rows {
		passed[r.Row] = r.Outcome == markers.OutcomePassed
	}
	var out []markers.Row
	for _, r := range d.Rows {
		if passed[r.ID] && r.Deliver != nil {
			out = append(out, r)
		}
	}
	return out
}

// checkDeliver rejects a delivered row whose deliver is unusable for issue creation: the rule
// pm_draft enforces on submission, applied to every row the publish would turn into an issue. The
// error names the row and the field, so the panel fails before any GitHub write.
func checkDeliver(r markers.Row, repos []config.CodeRepo) error {
	fields := []struct {
		name  string
		value string
	}{{"deliver.repo", r.Deliver.Repo}, {"deliver.title", r.Deliver.Title}, {"deliver.body", r.Deliver.Body}}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("row %s: %s is blank; a delivered row needs a non-blank repo, title and body", r.ID, f.name)
		}
	}
	if !slices.ContainsFunc(repos, func(c config.CodeRepo) bool { return strings.EqualFold(c.Repo, r.Deliver.Repo) }) {
		return fmt.Errorf("row %s: %s %s is not a code repository of shared/repos.yaml", r.ID, "deliver.repo", r.Deliver.Repo)
	}
	return nil
}

// replacement is the index in deliver of the row whose issue replaces the escalated
// implementation issue: the first one in that issue's repository when the draft settles the ask
// by replacing it; -1 otherwise.
func (pb *publish) replacement(deliver []markers.Row) int {
	s := pb.p.Draft.Settle
	if pb.esc == nil || s == nil || s.Kind != markers.SettleReplace {
		return -1
	}
	escRepo, _, ok := queue.ParseRef(pb.esc.Issue)
	if !ok {
		return -1
	}
	return slices.IndexFunc(deliver, func(r markers.Row) bool { return strings.EqualFold(r.Deliver.Repo, escRepo) })
}

// implIssues makes sure each row of deliver has its implementation issue (contract §7): found
// among the agenda's sub-issues by repository and title, or created as one; its body carries the
// numbered launch link; it is a Project item with Kind=Implementation and Proposer set, and
// Status=Todo when it has none.
func (pb *publish) implIssues(ctx context.Context, deliver []markers.Row, replacement int) ([]string, error) {
	refs := []string{}
	if len(deliver) == 0 {
		return refs, nil
	}
	subs, err := pb.gh.SubIssues(ctx, pb.repo, pb.number)
	if err != nil {
		return nil, err
	}
	for k, r := range deliver {
		code, ok := pb.codeRepo(r.Deliver.Repo)
		if !ok {
			return nil, fmt.Errorf("row %s delivers to %s, which is not a code repository of shared/repos.yaml", r.ID, r.Deliver.Repo)
		}
		d := config.ImplIssueData{Ref: string(pb.p.Ref), Agenda: pb.p.Agenda, Row: r}
		if k == replacement {
			d.Replaces = &config.ReplacesData{Old: pb.esc.Issue, Ask: pb.esc.Ask}
		}
		title, err := pb.cfg.Council.ImplIssue.RenderTitle(d)
		if err != nil {
			return nil, fmt.Errorf("implIssue.title of row %s: %w", r.ID, err)
		}
		i := slices.IndexFunc(subs, func(s github.SubIssue) bool { return strings.EqualFold(s.Repo, code.Repo) && s.Title == title })
		var is github.SubIssue
		if i >= 0 {
			is = subs[i]
		} else {
			body, err := pb.implBody(d)
			if err != nil {
				return nil, err
			}
			is, err = pb.gh.CreateIssue(ctx, github.NewIssue{Repo: code.Repo, Title: title, Body: body, ParentID: pb.agenda.NodeID, ProjectID: pb.project.ID})
			if err != nil {
				return nil, err
			}
			slog.Info("implementation issue created", "issue", pb.p.Ref, "row", r.ID, "impl", queue.FormatRef(is.Repo, is.Number))
		}
		if d.Link, err = pb.cfg.Council.LaunchLink.Link(code.Repo, is.Number, code.Cwd); err != nil {
			return nil, err
		}
		body, err := pb.implBody(d)
		if err != nil {
			return nil, err
		}
		if is.Body != body {
			if err := pb.gh.EditIssueBody(ctx, is.Repo, is.Number, body); err != nil {
				return nil, err
			}
		}
		if err := pb.implItem(ctx, is); err != nil {
			return nil, err
		}
		refs = append(refs, string(queue.FormatRef(code.Repo, is.Number)))
	}
	return refs, nil
}

func (pb *publish) codeRepo(repo string) (config.CodeRepo, bool) {
	i := slices.IndexFunc(pb.cfg.Repos.Code, func(c config.CodeRepo) bool { return strings.EqualFold(c.Repo, repo) })
	if i < 0 {
		return config.CodeRepo{}, false
	}
	return pb.cfg.Repos.Code[i], true
}

// implBody renders the body; a replacing issue's body ends with its pm:replaces marker.
func (pb *publish) implBody(d config.ImplIssueData) (string, error) {
	body, err := pb.cfg.Council.ImplIssue.RenderBody(d)
	if err != nil {
		return "", fmt.Errorf("implIssue.body of row %s: %w", d.Row.ID, err)
	}
	if d.Replaces == nil {
		return body, nil
	}
	return markers.Comment(body, markers.Replaces{Old: d.Replaces.Old, Ask: &d.Replaces.Ask})
}

func (pb *publish) implItem(ctx context.Context, is github.SubIssue) error {
	it, ok, err := pb.gh.IssueItem(ctx, is.Repo, is.Number, pb.project.ID)
	if err != nil {
		return err
	}
	if !ok {
		id, err := pb.gh.AddProjectItem(ctx, pb.project.ID, is.NodeID)
		if err != nil {
			return err
		}
		it = github.Item{ID: id, Fields: map[string]string{}}
	}
	f := pb.cfg.Project.Fields
	for _, s := range []struct {
		field     github.Select
		name, opt string
		onlyUnset bool // Status: dispatch moves it on; set only the first value
	}{
		{pb.project.Kind, f.Kind.Name, f.Kind.Options.Implementation, false},
		{pb.project.Proposer, f.Proposer.Name, f.Proposer.Options.CouncilSeat, false},
		{pb.project.Status, f.Status.Name, f.Status.Options.Todo, true},
	} {
		cur := it.Fields[s.name]
		if cur == s.opt || s.onlyUnset && cur != "" {
			continue
		}
		if err := pb.gh.SetOption(ctx, pb.project.ID, it.ID, s.field.ID, s.field.Options[s.opt]); err != nil {
			return err
		}
	}
	return nil
}

// supersedable splits the final draft's supersedes into the issues this human issue changes and
// refusals: a human issue, a decided issue, or one that is not an agenda issue of the Project.
// Only a human issue whose draft is not withdrawn changes issues.
func (pb *publish) supersedable(ctx context.Context) (accepted, refusals []string, err error) {
	accepted = []string{}
	if pb.p.Draft.Withdrawn != nil || len(pb.p.Draft.Supersedes) == 0 {
		return accepted, nil, nil
	}
	it, ok, err := pb.gh.IssueItem(ctx, pb.repo, pb.number, pb.project.ID)
	if err != nil {
		return nil, nil, err
	}
	f := pb.cfg.Project.Fields
	if !ok || it.Fields[f.Kind.Name] != f.Kind.Options.Human {
		return accepted, nil, nil
	}
	for _, ref := range pb.p.Draft.Supersedes {
		why, err := pb.refusal(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		if why == "" {
			accepted = append(accepted, ref)
		} else {
			refusals = append(refusals, fmt.Sprintf("supersede %s refused: %s", ref, why))
		}
	}
	return accepted, refusals, nil
}

func (pb *publish) refusal(ctx context.Context, ref string) (string, error) {
	repo, n, ok := queue.ParseRef(ref)
	switch {
	case !ok:
		return "not an issue reference", nil
	case !strings.EqualFold(repo, pb.repo):
		return "not an issue of the PM repository", nil
	case n == pb.number:
		return "the issue itself", nil
	}
	it, ok, err := pb.gh.IssueItem(ctx, repo, n, pb.project.ID)
	if err != nil {
		return "", err
	}
	f := pb.cfg.Project.Fields
	switch {
	case !ok:
		return "not in the Project", nil
	case it.Fields[f.Kind.Name] == f.Kind.Options.Human:
		return "a human issue", nil
	case it.Fields[f.Kind.Name] != f.Kind.Options.Agenda:
		return "not an agenda issue", nil
	case it.Fields[f.Status.Name] == f.Status.Options.Done:
		return "already decided", nil
	}
	bodies, err := pb.gh.Comments(ctx, repo, n)
	if err != nil {
		return "", err
	}
	ms, _ := markers.Collect(bodies)
	if slices.ContainsFunc(ms, func(m markers.Marker) bool { return m.Kind() == markers.KindDecision }) {
		return "already decided", nil
	}
	return "", nil
}

// withRefusals appends every refusal to every row's reason.
func withRefusals(rows []markers.DecisionRow, refusals []string) []markers.DecisionRow {
	out := slices.Clone(rows)
	if out == nil {
		out = []markers.DecisionRow{}
	}
	for i := range out {
		for _, r := range refusals {
			out[i].Reason += "; " + r
		}
	}
	return out
}

// supersede marks, sets Status=Superseded and closes as not planned each issue the decision
// changes, skipping what is already done.
func (pb *publish) supersede(ctx context.Context, refs []string) error {
	for _, ref := range refs {
		repo, n, ok := queue.ParseRef(ref)
		if !ok {
			return fmt.Errorf("superseded %q is not owner/repo#n", ref)
		}
		bodies, err := pb.gh.Comments(ctx, repo, n)
		if err != nil {
			return err
		}
		ms, _ := markers.Collect(bodies)
		if !slices.ContainsFunc(ms, func(m markers.Marker) bool { return m.Kind() == markers.KindSuperseded }) {
			text := fmt.Sprintf("本议题被人类议题 %s 变更，不再审议。", pb.p.Ref)
			if _, err := pb.post(ctx, repo, n, text, markers.Superseded{By: string(pb.p.Ref)}); err != nil {
				return err
			}
			slog.Info("superseded posted", "issue", ref, "by", pb.p.Ref)
		}
		if err := pb.closeAs(ctx, repo, n, pb.cfg.Project.Fields.Status.Options.Superseded, github.NotPlanned); err != nil {
			return err
		}
	}
	return nil
}

// closeAs sets the issue's Status to status when it is a Project item, then closes it with reason
// when it is open.
func (pb *publish) closeAs(ctx context.Context, repo string, n int, status string, reason github.CloseReason) error {
	it, ok, err := pb.gh.IssueItem(ctx, repo, n, pb.project.ID)
	if err != nil {
		return err
	}
	if ok && it.Fields[pb.cfg.Project.Fields.Status.Name] != status {
		if err := pb.gh.SetOption(ctx, pb.project.ID, it.ID, pb.project.Status.ID, pb.project.Status.Options[status]); err != nil {
			return err
		}
	}
	is, err := pb.gh.Issue(ctx, repo, n)
	if err != nil {
		return err
	}
	if !is.Closed {
		return pb.gh.CloseIssue(ctx, repo, n, reason)
	}
	return nil
}

// settle settles the ask the agenda issue was raised from exactly once (contract §2, §B): a
// replacing implementation issue was created in step 1, so the old issue gets pm:replaced, is
// superseded and closed; otherwise it gets a pm:reply naming this agenda issue. A reply or
// replaced already naming the ask ends it (a replaced one is still superseded and closed).
func (pb *publish) settle(ctx context.Context, dec markers.Decision, replacement int) error {
	if pb.esc == nil {
		return nil
	}
	repo, n, ok := queue.ParseRef(pb.esc.Issue)
	if !ok {
		return fmt.Errorf("escalation issue %q is not owner/repo#n", pb.esc.Issue)
	}
	bodies, err := pb.gh.Comments(ctx, repo, n)
	if err != nil {
		return err
	}
	replied, replaced := false, false
	for _, b := range bodies {
		switch m, _ := markers.Parse(b); m := m.(type) {
		case markers.Reply:
			replied = replied || m.Ask == pb.esc.Ask
		case markers.Replaced:
			replaced = replaced || m.Ask != nil && *m.Ask == pb.esc.Ask
		}
	}
	switch {
	case replied:
		return nil
	case replaced || replacement >= 0:
		if !replaced {
			if replacement >= len(dec.Impl) {
				return fmt.Errorf("decision of %s lists %d implementation issues; the replacement is number %d", pb.p.Ref, len(dec.Impl), replacement+1)
			}
			by := dec.Impl[replacement]
			text := fmt.Sprintf("本 issue 被 %s 取代：议题 %s 定案时这样了结询问 %d。", by, pb.p.Ref, pb.esc.Ask)
			if _, err := pb.post(ctx, repo, n, text, markers.Replaced{Ask: &pb.esc.Ask, By: by}); err != nil {
				return err
			}
			slog.Info("ask settled: replaced", "issue", pb.esc.Issue, "ask", pb.esc.Ask, "by", by)
		}
		return pb.closeAs(ctx, repo, n, pb.cfg.Project.Fields.Status.Options.Superseded, github.NotPlanned)
	}
	text := ""
	if s := pb.p.Draft.Settle; s != nil {
		text = s.Text
	}
	body, err := pb.cfg.Council.Reply.RenderBody(config.ReplyData{Ref: string(pb.p.Ref), Agenda: pb.p.Agenda, Text: text, Table: decisionTable(dec)})
	if err != nil {
		return fmt.Errorf("reply.body: %w", err)
	}
	if _, err := pb.post(ctx, repo, n, body, markers.Reply{Ask: pb.esc.Ask, Decision: new(string(pb.p.Ref))}); err != nil {
		return err
	}
	slog.Info("ask settled: reply", "issue", pb.esc.Issue, "ask", pb.esc.Ask, "decision", pb.p.Ref)
	return nil
}

// archive commits the decision file and one file per voting seat to the PM repository's default
// branch, unless the decision file already exists there.
func (pb *publish) archive(ctx context.Context, dec markers.Decision, ms []markers.Marker) error {
	a := pb.cfg.Council.Archive
	if a == nil {
		return nil
	}
	name := pb.p.Agenda
	if name == "" {
		name = strconv.Itoa(pb.number)
	}
	branch, err := pb.gh.DefaultBranch(ctx, pb.repo)
	if err != nil {
		return err
	}
	decisionPath := path.Join(a.Decisions, name+".md")
	exists, err := pb.gh.FileExists(ctx, pb.repo, branch, decisionPath)
	if err != nil || exists {
		return err
	}
	files := map[string]string{
		decisionPath: fmt.Sprintf("# %s %s\n\n%s\n\n%s\n%s", name, pb.agenda.Title, pb.p.Ref, DraftText(pb.p.Draft), decisionText(dec, nil)),
	}
	for _, m := range ms {
		if v, ok := m.(markers.Vote); ok {
			p := path.Join(a.Votes, name, v.Seat+".md")
			files[p] += voteText(v) + "\n"
		}
	}
	sha, err := pb.gh.CommitFiles(ctx, pb.repo, branch, fmt.Sprintf("Archive the decision of %s (%s)", name, pb.p.Ref), files)
	if err != nil {
		return err
	}
	slog.Info("decision archived", "issue", pb.p.Ref, "commit", sha, "files", len(files))
	return nil
}

// finish closes the agenda issue as completed, then sets Status=Done: Done is the last write, so
// a decided issue without it still has its publish to finish.
func (pb *publish) finish(ctx context.Context) error {
	if !pb.agenda.Closed {
		if err := pb.gh.CloseIssue(ctx, pb.repo, pb.number, github.Completed); err != nil {
			return err
		}
	}
	it, ok, err := pb.gh.IssueItem(ctx, pb.repo, pb.number, pb.project.ID)
	if err != nil {
		return err
	}
	done := pb.cfg.Project.Fields.Status.Options.Done
	if ok && it.Fields[pb.cfg.Project.Fields.Status.Name] != done {
		if err := pb.gh.SetOption(ctx, pb.project.ID, it.ID, pb.project.Status.ID, pb.project.Status.Options[done]); err != nil {
			return err
		}
	}
	slog.Info("agenda issue decided", "issue", pb.p.Ref, "status", done)
	return nil
}

// DraftText is the human-readable body of a pm:draft comment.
func DraftText(d markers.Draft) string {
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

// decisionTable is the decision's row table.
func decisionTable(d markers.Decision) string {
	var b strings.Builder
	b.WriteString("| 行 | 结果 | 赞成 | 门槛 | 理由 |\n|---|---|---|---|---|\n")
	for _, r := range d.Rows {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", cell.Replace(r.Row), r.Outcome, r.Approve, r.Threshold, cell.Replace(r.Reason))
	}
	return b.String()
}

// decisionText is the human-readable body of a pm:decision comment, with one line per refused
// supersede.
func decisionText(d markers.Decision, refusals []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "决策表第 %d 版定案。\n\n%s", d.Version, decisionTable(d))
	if len(d.Impl) > 0 {
		fmt.Fprintf(&b, "\n实现 issue：%s\n", strings.Join(d.Impl, "、"))
	}
	if len(d.Superseded) > 0 {
		fmt.Fprintf(&b, "\n变更的旧议题：%s\n", strings.Join(d.Superseded, "、"))
	}
	if len(refusals) > 0 {
		b.WriteString("\n")
		for _, r := range refusals {
			fmt.Fprintf(&b, "未变更：%s\n", r)
		}
	}
	return b.String()
}
