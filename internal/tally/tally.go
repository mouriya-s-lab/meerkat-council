// Package tally is the council's tally component. Its entry RecordVote takes a seat's vote from
// the council plugin's pm_vote and records it on the agenda issue as a pm:vote marker.
package tally

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/oh-my-omp/pm-council/internal/markers"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// GitHub is the part of the GitHub client the tally uses.
type GitHub interface {
	Comments(ctx context.Context, repo string, number int) ([]string, error)
	PostComment(ctx context.Context, repo string, number int, body string) (int64, error)
}

type Tally struct {
	gh GitHub
}

func New(gh GitHub) *Tally { return &Tally{gh: gh} }

// RecordVote posts seat's pm:vote on draft version of the issue ref. It re-reads the issue's markers
// first and drops the vote when that seat already voted on that version.
func (t *Tally) RecordVote(ctx context.Context, ref queue.Ref, seat string, version int, rows []markers.VoteRow) error {
	repo, n, ok := queue.ParseRef(string(ref))
	if !ok {
		return fmt.Errorf("record vote: bad issue ref %q", ref)
	}
	bodies, err := t.gh.Comments(ctx, repo, n)
	if err != nil {
		return err
	}
	ms, _ := markers.Collect(bodies)
	for _, m := range ms {
		if v, ok := m.(markers.Vote); ok && v.Seat == seat && v.Version == version {
			slog.Info("vote dropped: seat already voted on this version", "issue", ref, "seat", seat, "version", version)
			return nil
		}
	}
	vote := markers.Vote{Seat: seat, Version: version, Rows: rows}
	body, err := markers.Comment(voteText(vote), vote)
	if err != nil {
		return err
	}
	id, err := t.gh.PostComment(ctx, repo, n, body)
	if err != nil {
		return err
	}
	slog.Info("vote recorded", "issue", ref, "seat", seat, "version", version, "comment", id)
	return nil
}

var cell = strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>")

func voteText(v markers.Vote) string {
	var b strings.Builder
	fmt.Fprintf(&b, "席位 %s 对决策表第 %d 版的投票。\n\n| 行 | 票 | 反例 | 引用 | 分类意见 |\n|---|---|---|---|---|\n", v.Seat, v.Version)
	opt := func(s *string) string {
		if s == nil {
			return ""
		}
		return cell.Replace(*s)
	}
	for _, r := range v.Rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", cell.Replace(r.Row), r.Choice, opt(r.Counterexample), opt(r.Citation), opt(r.Classification))
	}
	return b.String()
}
