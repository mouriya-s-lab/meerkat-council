package tally

import (
	"fmt"
	"slices"
	"strings"

	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/markers"
)

// Outcome is what a draft version's votes lead to: Arbitrate, Revise or Decide.
type Outcome interface{ outcome() }

// Arbitrate: some rows have a category dispute without a ruling on this version.
type Arbitrate struct {
	Disputes []Dispute
}

// Dispute is a row whose category a veto seat disputes by vetoing it outside the veto categories.
type Dispute struct {
	Row                      markers.Row
	Seat                     string
	Citation, Classification *string
}

// Revise: some rows are opposed with a counterexample and the revision limit is not reached.
type Revise struct {
	Rows            []string
	Counterexamples []Counterexample
}

// Counterexample is one seat's counterexample against one row.
type Counterexample struct {
	Seat, Row, Text string
}

// Decide: every row has its outcome.
type Decide struct {
	Rows []markers.DecisionRow
}

func (Arbitrate) outcome() {}
func (Revise) outcome()    {}
func (Decide) outcome()    {}

// Evaluate tallies the votes on draft in round. Only votes and rulings on the draft's version
// count; a later vote of a seat or ruling of a row replaces an earlier one. humanRefs are the
// human issues of the PM repository, the only citations a veto can stand on.
//
// Per row: a standing veto (a veto seat citing a human issue on a row whose effective category,
// the ruling's or else the draft's, is a veto category, or that misses design) rejects it; a veto
// seat's veto on any other row without a ruling disputes the category; other vetoes count neither
// way; an oppose with a counterexample revises the draft while round < maxRounds and rejects the
// row after; otherwise the row passes when approvals reach the threshold. Any dispute makes the
// outcome Arbitrate, else any revision Revise, else Decide.
func Evaluate(draft markers.Draft, votes []markers.Vote, rulings []markers.Ruling, seats []config.Seat, cfg config.Tally, humanRefs []string, round int) Outcome {
	eligible := voters(seats)
	threshold := cfg.Threshold.Of(len(eligible))
	ballots := ballotsOn(draft.Version, votes, eligible)
	ruled := map[string]string{}
	for _, r := range rulings {
		if r.Version != draft.Version {
			continue
		}
		for _, row := range r.Rows {
			ruled[row.Row] = row.Category
		}
	}
	human := map[string]bool{}
	for _, h := range humanRefs {
		human[normRef(h)] = true
	}

	approvals := func(row string) int {
		n := 0
		for _, s := range eligible {
			if b, ok := ballots[s][row]; ok && b.Choice == markers.ChoiceApprove {
				n++
			}
		}
		return n
	}

	if draft.Withdrawn != nil {
		rows := make([]markers.DecisionRow, 0, len(draft.Rows))
		for _, r := range draft.Rows {
			rows = append(rows, markers.DecisionRow{Row: r.ID, Outcome: markers.OutcomeWithdrawn, Approve: approvals(r.ID), Threshold: threshold, Reason: *draft.Withdrawn})
		}
		return Decide{Rows: rows}
	}

	var (
		disputes []Dispute
		revise   []string
		decided  = make([]markers.DecisionRow, 0, len(draft.Rows))
	)
	for _, r := range draft.Rows {
		approve := approvals(r.ID)
		dr := markers.DecisionRow{Row: r.ID, Approve: approve, Threshold: threshold}
		category, hasRuling := ruled[r.ID]
		if !hasRuling {
			category = r.Category
		}
		vetoable := slices.Contains(cfg.Veto.Categories, category) || r.MissingDesign

		var standing, dispute *string
		for _, s := range eligible {
			b, ok := ballots[s][r.ID]
			if !ok || b.Choice != markers.ChoiceVeto || !slices.Contains(cfg.Veto.Seats, s) {
				continue
			}
			switch {
			case vetoable && b.Citation != nil && human[normRef(*b.Citation)]:
				standing = new(fmt.Sprintf("veto by %s citing %s", s, *b.Citation))
			case !vetoable && !hasRuling && dispute == nil:
				dispute = &s
				disputes = append(disputes, Dispute{Row: r, Seat: s, Citation: b.Citation, Classification: b.Classification})
			}
			if standing != nil {
				break
			}
		}
		opposed := slices.ContainsFunc(eligible, func(s string) bool {
			b, ok := ballots[s][r.ID]
			return ok && b.Choice == markers.ChoiceOppose && hasText(b.Counterexample)
		})
		switch {
		case standing != nil:
			dr.Outcome, dr.Reason = markers.OutcomeRejected, *standing
		case dispute != nil:
			continue
		case opposed && round < cfg.MaxRounds:
			revise = append(revise, r.ID)
			continue
		case opposed:
			dr.Outcome, dr.Reason = markers.OutcomeRejected, fmt.Sprintf("revision limit %d reached", cfg.MaxRounds)
		case approve >= threshold:
			dr.Outcome, dr.Reason = markers.OutcomePassed, fmt.Sprintf("approve %d meets threshold %d", approve, threshold)
		default:
			dr.Outcome, dr.Reason = markers.OutcomeRejected, fmt.Sprintf("approve %d below threshold %d", approve, threshold)
		}
		decided = append(decided, dr)
	}
	switch {
	case len(disputes) > 0:
		return Arbitrate{Disputes: disputes}
	case len(revise) > 0:
		return Revise{Rows: revise, Counterexamples: Counterexamples(draft, votes, seats)}
	}
	return Decide{Rows: decided}
}

// Counterexamples are the voting seats' counterexamples against rows of draft, in row then seat
// order.
func Counterexamples(draft markers.Draft, votes []markers.Vote, seats []config.Seat) []Counterexample {
	eligible := voters(seats)
	ballots := ballotsOn(draft.Version, votes, eligible)
	var out []Counterexample
	for _, r := range draft.Rows {
		for _, s := range eligible {
			if b, ok := ballots[s][r.ID]; ok && b.Choice == markers.ChoiceOppose && hasText(b.Counterexample) {
				out = append(out, Counterexample{Seat: s, Row: r.ID, Text: *b.Counterexample})
			}
		}
	}
	return out
}

// voters are the ids of the seats with the vote right, in seat order.
func voters(seats []config.Seat) []string {
	var ids []string
	for _, s := range seats {
		if s.Has(config.RightVote) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// ballotsOn maps each eligible seat's latest vote on version to its rows by row id.
func ballotsOn(version int, votes []markers.Vote, eligible []string) map[string]map[string]markers.VoteRow {
	out := map[string]map[string]markers.VoteRow{}
	for _, v := range votes {
		if v.Version != version || !slices.Contains(eligible, v.Seat) {
			continue
		}
		rows := map[string]markers.VoteRow{}
		for _, r := range v.Rows {
			rows[r.Row] = r
		}
		out[v.Seat] = rows
	}
	return out
}

func hasText(s *string) bool { return s != nil && strings.TrimSpace(*s) != "" }

func normRef(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
