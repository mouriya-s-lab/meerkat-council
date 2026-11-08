package tally

import (
	"reflect"
	"testing"

	"github.com/mouriya-s-lab/meerkat-council/internal/config"
	"github.com/mouriya-s-lab/meerkat-council/internal/markers"
)

var (
	testSeats = []config.Seat{
		{ID: "Dr", Rights: []config.Right{config.RightDraft, config.RightVote}},
		{ID: "Hi", Rights: []config.Right{config.RightVote}},
		{ID: "Ve", Rights: []config.Right{config.RightVote, config.RightVeto}},
		{ID: "Ar", Rights: []config.Right{config.RightVote, config.RightArbitrate}},
	}
	testTally = config.Tally{
		Threshold:   config.Threshold{Numerator: 3, Denominator: 4},
		Veto:        config.Veto{Seats: []string{"Ve"}, Categories: []string{"intent"}},
		Arbitration: config.Arbitration{Seat: "Ar"},
		MaxRounds:   2,
		Categories:  []string{"intent", "tech"},
	}
	testHumans = []string{"o/pm#9"}
)

func row(id, category string, missingDesign bool) markers.Row {
	return markers.Row{ID: id, What: "w", Where: "p", How: "h", Tradeoff: "t", Category: category, MissingDesign: missingDesign}
}

// ballot is one seat's vote on version with the same choice on every listed row.
func ballot(seat string, version int, rows ...markers.VoteRow) markers.Vote {
	return markers.Vote{Seat: seat, Version: version, Rows: rows}
}

func approve(r string) markers.VoteRow { return markers.VoteRow{Row: r, Choice: markers.ChoiceApprove} }

func oppose(r, counterexample string) markers.VoteRow {
	vr := markers.VoteRow{Row: r, Choice: markers.ChoiceOppose}
	if counterexample != "" {
		vr.Counterexample = &counterexample
	}
	return vr
}

func veto(r, citation string) markers.VoteRow {
	return markers.VoteRow{Row: r, Choice: markers.ChoiceVeto, Citation: &citation, Classification: new("tech")}
}

func allApprove(version int, rows ...string) []markers.Vote {
	var out []markers.Vote
	for _, s := range []string{"Dr", "Hi", "Ve", "Ar"} {
		var vr []markers.VoteRow
		for _, r := range rows {
			vr = append(vr, approve(r))
		}
		out = append(out, ballot(s, version, vr...))
	}
	return out
}

func decided(rows ...markers.DecisionRow) Decide { return Decide{Rows: rows} }

func passed(r string, approve int) markers.DecisionRow {
	return markers.DecisionRow{Row: r, Outcome: markers.OutcomePassed, Approve: approve, Threshold: 3, Reason: "approve " + itoa(approve) + " meets threshold 3"}
}

func rejected(r string, approve int, reason string) markers.DecisionRow {
	return markers.DecisionRow{Row: r, Outcome: markers.OutcomeRejected, Approve: approve, Threshold: 3, Reason: reason}
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestEvaluate(t *testing.T) {
	r1, r2 := row("D1-01", "intent", false), row("D1-02", "tech", false)
	draft := func(rows ...markers.Row) markers.Draft {
		return markers.Draft{Version: 1, Rows: rows, Supersedes: []string{}}
	}
	ruling := func(version int, r, category string) markers.Ruling {
		return markers.Ruling{Seat: "Ar", Version: version, Rows: []markers.RulingRow{{Row: r, Category: category}}}
	}
	cases := []struct {
		name    string
		draft   markers.Draft
		votes   []markers.Vote
		rulings []markers.Ruling
		round   int
		want    Outcome
	}{
		{
			name:  "every seat approves: passed",
			draft: draft(r1), votes: allApprove(1, "D1-01"), round: 1,
			want: decided(passed("D1-01", 4)),
		},
		{
			name:  "approvals below threshold, vetoes not standing: rejected by threshold",
			draft: draft(r1), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01")), ballot("Hi", 1, approve("D1-01")),
				ballot("Ve", 1, veto("D1-01", "o/pm#3")), ballot("Ar", 1, veto("D1-01", "o/pm#9")),
			},
			want: decided(rejected("D1-01", 2, "approve 2 below threshold 3")),
		},
		{
			name:  "veto seat cites a human issue on a veto category: veto stands",
			draft: draft(r1), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01")), ballot("Hi", 1, approve("D1-01")),
				ballot("Ve", 1, veto("D1-01", "o/pm#9")), ballot("Ar", 1, approve("D1-01")),
			},
			want: decided(rejected("D1-01", 3, "veto by Ve citing o/pm#9")),
		},
		{
			name:  "human citation compares case-insensitively",
			draft: draft(r1), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01")), ballot("Hi", 1, approve("D1-01")),
				ballot("Ve", 1, veto("D1-01", " O/PM#9 ")), ballot("Ar", 1, approve("D1-01")),
			},
			want: decided(rejected("D1-01", 3, "veto by Ve citing  O/PM#9 ")),
		},
		{
			name:  "veto citing an agenda issue does not stand and counts neither way",
			draft: draft(r1), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01")), ballot("Hi", 1, approve("D1-01")),
				ballot("Ve", 1, veto("D1-01", "o/pm#3")), ballot("Ar", 1, approve("D1-01")),
			},
			want: decided(passed("D1-01", 3)),
		},
		{
			name:  "missing design makes any category vetoable",
			draft: draft(row("D1-01", "tech", true)), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01")), ballot("Hi", 1, approve("D1-01")),
				ballot("Ve", 1, veto("D1-01", "o/pm#9")), ballot("Ar", 1, approve("D1-01")),
			},
			want: decided(rejected("D1-01", 3, "veto by Ve citing o/pm#9")),
		},
		{
			name:  "veto seat vetoes a non-veto category without a ruling: arbitrate",
			draft: draft(r1, r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01"), approve("D1-02")), ballot("Hi", 1, approve("D1-01"), approve("D1-02")),
				ballot("Ve", 1, approve("D1-01"), veto("D1-02", "o/pm#9")), ballot("Ar", 1, approve("D1-01"), approve("D1-02")),
			},
			want: Arbitrate{Disputes: []Dispute{{Row: r2, Seat: "Ve", Citation: new("o/pm#9"), Classification: new("tech")}}},
		},
		{
			name:  "a ruling on another version does not settle the dispute",
			draft: draft(r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-02")), ballot("Hi", 1, approve("D1-02")),
				ballot("Ve", 1, veto("D1-02", "o/pm#9")), ballot("Ar", 1, approve("D1-02")),
			},
			rulings: []markers.Ruling{ruling(2, "D1-02", "tech")},
			want:    Arbitrate{Disputes: []Dispute{{Row: r2, Seat: "Ve", Citation: new("o/pm#9"), Classification: new("tech")}}},
		},
		{
			name:  "ruled outside the veto categories: veto void, no second arbitration",
			draft: draft(r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-02")), ballot("Hi", 1, approve("D1-02")),
				ballot("Ve", 1, veto("D1-02", "o/pm#9")), ballot("Ar", 1, approve("D1-02")),
			},
			rulings: []markers.Ruling{ruling(1, "D1-02", "tech")},
			want:    decided(passed("D1-02", 3)),
		},
		{
			name:  "ruled into a veto category: veto stands",
			draft: draft(r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-02")), ballot("Hi", 1, approve("D1-02")),
				ballot("Ve", 1, veto("D1-02", "o/pm#9")), ballot("Ar", 1, approve("D1-02")),
			},
			rulings: []markers.Ruling{ruling(1, "D1-02", "intent")},
			want:    decided(rejected("D1-02", 3, "veto by Ve citing o/pm#9")),
		},
		{
			name:  "the latest ruling of a row wins",
			draft: draft(r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-02")), ballot("Hi", 1, approve("D1-02")),
				ballot("Ve", 1, veto("D1-02", "o/pm#9")), ballot("Ar", 1, approve("D1-02")),
			},
			rulings: []markers.Ruling{ruling(1, "D1-02", "intent"), ruling(1, "D1-02", "tech")},
			want:    decided(passed("D1-02", 3)),
		},
		{
			name:  "ruled into a veto category with a non-human citation: veto void",
			draft: draft(r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-02")), ballot("Hi", 1, approve("D1-02")),
				ballot("Ve", 1, veto("D1-02", "o/pm#3")), ballot("Ar", 1, approve("D1-02")),
			},
			rulings: []markers.Ruling{ruling(1, "D1-02", "intent")},
			want:    decided(passed("D1-02", 3)),
		},
		{
			name:  "a veto by a seat outside veto.seats neither stands nor disputes",
			draft: draft(r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-02")), ballot("Hi", 1, approve("D1-02")),
				ballot("Ve", 1, approve("D1-02")), ballot("Ar", 1, veto("D1-02", "o/pm#9")),
			},
			want: decided(passed("D1-02", 3)),
		},
		{
			name:  "oppose with a counterexample before the round limit: revise",
			draft: draft(r1, r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01"), approve("D1-02")), ballot("Hi", 1, oppose("D1-01", "breaks x"), approve("D1-02")),
				ballot("Ve", 1, approve("D1-01"), approve("D1-02")), ballot("Ar", 1, approve("D1-01"), approve("D1-02")),
			},
			want: Revise{Rows: []string{"D1-01"}, Counterexamples: []Counterexample{{Seat: "Hi", Row: "D1-01", Text: "breaks x"}}},
		},
		{
			name:  "oppose with a counterexample at the round limit: rejected",
			draft: draft(r1, r2), round: 2,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01"), approve("D1-02")), ballot("Hi", 1, oppose("D1-01", "breaks x"), approve("D1-02")),
				ballot("Ve", 1, approve("D1-01"), approve("D1-02")), ballot("Ar", 1, approve("D1-01"), approve("D1-02")),
			},
			want: decided(rejected("D1-01", 3, "revision limit 2 reached"), passed("D1-02", 4)),
		},
		{
			name:  "oppose without a counterexample counts against the threshold only",
			draft: draft(r1), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01")), ballot("Hi", 1, oppose("D1-01", "")),
				ballot("Ve", 1, approve("D1-01")), ballot("Ar", 1, approve("D1-01")),
			},
			want: decided(passed("D1-01", 3)),
		},
		{
			name:  "a dispute takes precedence over a revision",
			draft: draft(r1, r2), round: 1,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01"), approve("D1-02")), ballot("Hi", 1, oppose("D1-01", "breaks x"), approve("D1-02")),
				ballot("Ve", 1, approve("D1-01"), veto("D1-02", "o/pm#9")), ballot("Ar", 1, approve("D1-01"), approve("D1-02")),
			},
			want: Arbitrate{Disputes: []Dispute{{Row: r2, Seat: "Ve", Citation: new("o/pm#9"), Classification: new("tech")}}},
		},
		{
			name:  "a standing veto decides its row even when another row is opposed",
			draft: draft(r1, r2), round: 2,
			votes: []markers.Vote{
				ballot("Dr", 1, approve("D1-01"), approve("D1-02")), ballot("Hi", 1, approve("D1-01"), oppose("D1-02", "y")),
				ballot("Ve", 1, veto("D1-01", "o/pm#9"), approve("D1-02")), ballot("Ar", 1, approve("D1-01"), approve("D1-02")),
			},
			want: decided(rejected("D1-01", 3, "veto by Ve citing o/pm#9"), rejected("D1-02", 3, "revision limit 2 reached")),
		},
		{
			name:  "withdrawn draft: every row withdrawn",
			draft: markers.Draft{Version: 1, Rows: []markers.Row{r1, r2}, Supersedes: []string{}, Withdrawn: new("no longer needed")},
			votes: allApprove(1, "D1-01", "D1-02"), round: 1,
			want: decided(
				markers.DecisionRow{Row: "D1-01", Outcome: markers.OutcomeWithdrawn, Approve: 4, Threshold: 3, Reason: "no longer needed"},
				markers.DecisionRow{Row: "D1-02", Outcome: markers.OutcomeWithdrawn, Approve: 4, Threshold: 3, Reason: "no longer needed"},
			),
		},
		{
			name:  "only votes on the draft's version count, the latest per seat",
			draft: markers.Draft{Version: 2, Rows: []markers.Row{r1}, Supersedes: []string{}}, round: 2,
			votes: append(allApprove(1, "D1-01"),
				ballot("Dr", 2, oppose("D1-01", "x")), ballot("Dr", 2, approve("D1-01")),
				ballot("Hi", 2, approve("D1-01")), ballot("Ve", 2, approve("D1-01"))),
			want: decided(passed("D1-01", 3)),
		},
		{
			name:  "a seat without the vote right does not count",
			draft: draft(r1), round: 1,
			votes: append(allApprove(1, "D1-01")[:2], ballot("Nobody", 1, approve("D1-01"))),
			want:  decided(rejected("D1-01", 2, "approve 2 below threshold 3")),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Evaluate(c.draft, c.votes, c.rulings, testSeats, testTally, testHumans, c.round)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Evaluate =\n%#v\nwant\n%#v", got, c.want)
			}
		})
	}
}

func TestThreshold(t *testing.T) {
	th := config.Threshold{Numerator: 3, Denominator: 4}
	for eligible, want := range map[int]int{4: 3, 5: 4, 6: 5, 7: 6, 1: 1} {
		if got := th.Of(eligible); got != want {
			t.Errorf("Of(%d) = %d, want %d", eligible, got, want)
		}
	}
}
