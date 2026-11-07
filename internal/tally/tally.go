// Package tally is the council's tally component: RecordVote and RecordRuling record a seat's
// pm_vote or pm_ruling as a marker on the agenda issue, Evaluate decides a draft version from its
// votes and rulings, and Publish carries a decision out on GitHub.
package tally

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// GitHub is the part of the GitHub client the tally uses.
type GitHub interface {
	Issue(ctx context.Context, repo string, number int) (github.Issue, error)
	Comments(ctx context.Context, repo string, number int) ([]string, error)
	CommentMarkers(ctx context.Context, repo string, number int, fn func(m markers.Marker) error) error
	PostComment(ctx context.Context, repo string, number int, body string) (int64, error)
	EditIssueBody(ctx context.Context, repo string, number int, body string) error
	CloseIssue(ctx context.Context, repo string, number int, reason github.CloseReason) error
	SubIssues(ctx context.Context, repo string, number int) ([]github.SubIssue, error)
	CreateIssue(ctx context.Context, n github.NewIssue) (github.SubIssue, error)
	IssueItem(ctx context.Context, repo string, number int, projectID string) (github.Item, bool, error)
	AddProjectItem(ctx context.Context, projectID, contentID string) (string, error)
	SetOption(ctx context.Context, projectID, itemID, fieldID, optionID string) error
	DefaultBranch(ctx context.Context, repo string) (string, error)
	FileExists(ctx context.Context, repo, branch, path string) (bool, error)
	CommitFiles(ctx context.Context, repo, branch, message string, files map[string]string) (string, error)
}

type Tally struct {
	cfg     *config.Config
	gh      GitHub
	project github.Project
}

func New(cfg *config.Config, gh GitHub, project github.Project) *Tally {
	return &Tally{cfg: cfg, gh: gh, project: project}
}

// RecordVote posts seat's pm:vote on draft version of the issue ref. It re-reads the issue's markers
// first and drops the vote when that seat already voted on that version.
func (t *Tally) RecordVote(ctx context.Context, ref queue.Ref, seat string, version int, rows []markers.VoteRow) error {
	repo, n, ms, err := t.markers(ctx, ref)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if v, ok := m.(markers.Vote); ok && v.Seat == seat && v.Version == version {
			slog.Info("vote dropped: seat already voted on this version", "issue", ref, "seat", seat, "version", version)
			return nil
		}
	}
	vote := markers.Vote{Seat: seat, Version: version, Rows: rows}
	id, err := t.post(ctx, repo, n, voteText(vote), vote)
	if err != nil {
		return err
	}
	slog.Info("vote recorded", "issue", ref, "seat", seat, "version", version, "comment", id)
	return nil
}

// RecordRuling posts seat's pm:ruling on draft version of the issue ref. It re-reads the issue's
// markers first and drops the ruling when every row it rules already has a ruling on that version.
func (t *Tally) RecordRuling(ctx context.Context, ref queue.Ref, seat string, version int, rows []markers.RulingRow) error {
	repo, n, ms, err := t.markers(ctx, ref)
	if err != nil {
		return err
	}
	ruled := map[string]bool{}
	for _, m := range ms {
		if r, ok := m.(markers.Ruling); ok && r.Version == version {
			for _, row := range r.Rows {
				ruled[row.Row] = true
			}
		}
	}
	pending := false
	for _, r := range rows {
		pending = pending || !ruled[r.Row]
	}
	if !pending {
		slog.Info("ruling dropped: its rows already have rulings on this version", "issue", ref, "seat", seat, "version", version)
		return nil
	}
	ruling := markers.Ruling{Seat: seat, Version: version, Rows: rows}
	id, err := t.post(ctx, repo, n, rulingText(ruling), ruling)
	if err != nil {
		return err
	}
	slog.Info("ruling recorded", "issue", ref, "seat", seat, "version", version, "comment", id)
	return nil
}

func (t *Tally) markers(ctx context.Context, ref queue.Ref) (string, int, []markers.Marker, error) {
	repo, n, ok := queue.ParseRef(string(ref))
	if !ok {
		return "", 0, nil, fmt.Errorf("bad issue ref %q", ref)
	}
	var ms []markers.Marker
	if err := t.gh.CommentMarkers(ctx, repo, n, func(m markers.Marker) error {
		ms = append(ms, m)
		return nil
	}); err != nil {
		return "", 0, nil, err
	}
	return repo, n, ms, nil
}

func (t *Tally) post(ctx context.Context, repo string, n int, text string, m markers.Marker) (int64, error) {
	body, err := markers.Comment(text, m)
	if err != nil {
		return 0, err
	}
	return t.gh.PostComment(ctx, repo, n, body)
}

var cell = strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>")

func opt(s *string) string {
	if s == nil {
		return ""
	}
	return cell.Replace(*s)
}

func voteText(v markers.Vote) string {
	var b strings.Builder
	fmt.Fprintf(&b, "席位 %s 对决策表第 %d 版的投票。\n\n| 行 | 票 | 反例 | 引用 | 分类意见 |\n|---|---|---|---|---|\n", v.Seat, v.Version)
	for _, r := range v.Rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", cell.Replace(r.Row), r.Choice, opt(r.Counterexample), opt(r.Citation), opt(r.Classification))
	}
	return b.String()
}

func rulingText(r markers.Ruling) string {
	var b strings.Builder
	fmt.Fprintf(&b, "席位 %s 对决策表第 %d 版分类争议的仲裁。\n\n| 行 | 分类 |\n|---|---|\n", r.Seat, r.Version)
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %s | %s |\n", cell.Replace(row.Row), cell.Replace(row.Category))
	}
	return b.String()
}
