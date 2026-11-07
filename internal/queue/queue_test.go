package queue

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/oh-my-omp/pm-council/internal/markers"
)

const repo = "o/pm"

func ref(n int) Ref { return FormatRef(repo, n) }

func issue(n int, k Kind, st Status, agenda string, ms ...markers.Marker) Issue {
	return Issue{Ref: ref(n), Number: n, Kind: k, Status: st, Agenda: agenda, Markers: ms}
}

func blocked(is Issue, by ...int) Issue {
	for _, n := range by {
		is.BlockedBy = append(is.BlockedBy, ref(n))
	}
	return is
}

func panelEv(ev markers.PanelEvent, round int) markers.Panel {
	return markers.Panel{Event: ev, Round: round, Sessions: []markers.PanelSession{}}
}

var seats = []Seat{{"Coordination", true}, {"ClaudeHigh", true}, {"Argon", true}, {"Drafter", false}}

func states(q Queue) map[Ref]State {
	m := map[Ref]State{}
	for _, e := range q {
		m[e.Ref] = e.State
	}
	return m
}

func entry(t *testing.T, q Queue, r Ref) Entry {
	t.Helper()
	for _, e := range q {
		if e.Ref == r {
			return e
		}
	}
	t.Fatalf("%s not in queue", r)
	return Entry{}
}

func TestStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issues []Issue
		want   map[Ref]State
	}{
		{
			name:   "agenda alone is ready",
			issues: []Issue{issue(1, KindAgenda, StatusTodo, "D1")},
			want:   map[Ref]State{ref(1): StateReady},
		},
		{
			name: "undecided human holds agenda issues",
			issues: []Issue{
				issue(1, KindAgenda, StatusTodo, "D1"),
				issue(2, KindAgenda, StatusInProgress, "D2", panelEv(markers.PanelStarted, 1)),
				issue(3, KindHuman, StatusTodo, "D3"),
			},
			want: map[Ref]State{ref(1): StateHeld, ref(2): StateHeld, ref(3): StateReady},
		},
		{
			name: "human blocked by undecided human",
			issues: []Issue{
				issue(1, KindHuman, StatusTodo, "D1"),
				blocked(issue(2, KindHuman, StatusTodo, "D2"), 1),
			},
			want: map[Ref]State{ref(1): StateReady, ref(2): StateBlocked},
		},
		{
			name: "prerequisite done unblocks; decided human no longer holds",
			issues: []Issue{
				issue(1, KindHuman, StatusDone, "D1"),
				blocked(issue(2, KindHuman, StatusTodo, "D2"), 1),
				issue(3, KindAgenda, StatusTodo, "D3"),
			},
			want: map[Ref]State{ref(1): StateDecided, ref(2): StateReady, ref(3): StateHeld},
		},
		{
			name: "decision marker decides",
			issues: []Issue{
				issue(1, KindHuman, StatusInProgress, "D1", markers.Decision{Version: 1}),
				issue(2, KindAgenda, StatusTodo, "D2"),
			},
			want: map[Ref]State{ref(1): StateDecided, ref(2): StateReady},
		},
		{
			name: "superseded by status or marker beats blocked and held",
			issues: []Issue{
				issue(1, KindHuman, StatusTodo, "D1"),
				issue(2, KindAgenda, StatusSuperseded, "D2"),
				blocked(issue(3, KindAgenda, StatusTodo, "D3", markers.Superseded{By: string(ref(1))}), 1),
			},
			want: map[Ref]State{ref(1): StateReady, ref(2): StateSuperseded, ref(3): StateSuperseded},
		},
		{
			name: "decided beats superseded",
			issues: []Issue{
				issue(1, KindAgenda, StatusDone, "D1", markers.Superseded{By: "o/pm#9"}),
			},
			want: map[Ref]State{ref(1): StateDecided},
		},
		{
			name: "blocked beats held",
			issues: []Issue{
				issue(1, KindHuman, StatusTodo, "D1"),
				blocked(issue(2, KindAgenda, StatusTodo, "D2"), 1),
			},
			want: map[Ref]State{ref(1): StateReady, ref(2): StateBlocked},
		},
		{
			name: "non-human prerequisite ignored",
			issues: []Issue{
				issue(1, KindAgenda, StatusTodo, "D1"),
				blocked(issue(2, KindAgenda, StatusTodo, "D2"), 1, 99),
			},
			want: map[Ref]State{ref(1): StateReady, ref(2): StateReady},
		},
		{
			name: "latest panel event decides",
			issues: []Issue{
				issue(1, KindAgenda, StatusInProgress, "D1", panelEv(markers.PanelStarted, 1)),
				issue(2, KindAgenda, StatusInProgress, "D2", panelEv(markers.PanelStarted, 1), panelEv(markers.PanelInterrupted, 1)),
				issue(3, KindAgenda, StatusInProgress, "D3", panelEv(markers.PanelInterrupted, 1), panelEv(markers.PanelResumed, 2)),
				issue(4, KindAgenda, StatusInProgress, "D4", panelEv(markers.PanelStarted, 1), panelEv(markers.PanelFailed, 1)),
				issue(5, KindAgenda, StatusTodo, "D5", panelEv(markers.PanelStarted, 1), panelEv(markers.PanelBlocked, 1)),
			},
			want: map[Ref]State{ref(1): StateDeliberating, ref(2): StateInterrupted, ref(3): StateDeliberating, ref(4): StateFailed, ref(5): StateReady},
		},
		{
			name: "issues without human or agenda kind are not on the queue",
			issues: []Issue{
				issue(1, KindNone, StatusNone, ""),
				issue(2, KindImplementation, StatusTodo, ""),
				issue(3, KindAgenda, StatusTodo, "D1"),
			},
			want: map[Ref]State{ref(3): StateReady},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := states(Build(Snapshot{Issues: tc.issues, Seats: seats, AgendaPrefix: "D"}))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("states = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlockedByAndNotes(t *testing.T) {
	q := Build(Snapshot{AgendaPrefix: "D", Issues: []Issue{
		issue(1, KindHuman, StatusTodo, "D1"),
		issue(2, KindAgenda, StatusTodo, "D2"),
		blocked(issue(3, KindHuman, StatusTodo, "D3"), 1),
		blocked(issue(4, KindAgenda, StatusTodo, "D4"), 2),
	}})
	if e := entry(t, q, ref(3)); !reflect.DeepEqual(e.BlockedBy, []Ref{ref(1)}) || len(e.Notes) != 0 {
		t.Errorf("#3 blockedBy %v notes %v", e.BlockedBy, e.Notes)
	}
	e := entry(t, q, ref(4))
	if len(e.BlockedBy) != 0 || len(e.Notes) != 1 || !strings.Contains(e.Notes[0], string(ref(2))+" ignored") {
		t.Errorf("#4 blockedBy %v notes %v", e.BlockedBy, e.Notes)
	}
	if e.Kind != "Agenda" || entry(t, q, ref(1)).Kind != "Human" {
		t.Errorf("kinds %q %q", e.Kind, entry(t, q, ref(1)).Kind)
	}
}

func TestPanelProgress(t *testing.T) {
	q := Build(Snapshot{AgendaPrefix: "D", Seats: seats, Issues: []Issue{
		issue(1, KindAgenda, StatusTodo, "D1"),
		issue(2, KindAgenda, StatusInProgress, "D2",
			panelEv(markers.PanelStarted, 1),
			markers.Draft{Version: 1},
			markers.Vote{Seat: "Argon", Version: 1},
			markers.Draft{Version: 2},
			markers.Vote{Seat: "ClaudeHigh", Version: 2},
			markers.Vote{Seat: "ClaudeHigh", Version: 2},
		),
		issue(3, KindAgenda, StatusInProgress, "D3", panelEv(markers.PanelStarted, 1)),
	}})
	if p := entry(t, q, ref(1)).Panel; p != nil {
		t.Errorf("#1 panel = %+v, want nil", p)
	}
	p := entry(t, q, ref(2)).Panel
	if p == nil || p.Round != 1 || p.Version == nil || *p.Version != 2 ||
		!reflect.DeepEqual(p.Votes, []string{"ClaudeHigh"}) || !reflect.DeepEqual(p.Pending, []string{"Coordination", "Argon"}) {
		t.Errorf("#2 panel = %+v", p)
	}
	p = entry(t, q, ref(3)).Panel
	if p == nil || p.Version != nil || len(p.Votes) != 0 || len(p.Pending) != 3 {
		t.Errorf("#3 panel = %+v", p)
	}
}

func TestOrder(t *testing.T) {
	refs := func(q Queue) []Ref {
		var out []Ref
		for _, e := range q {
			out = append(out, e.Ref)
		}
		return out
	}
	// After the human issues are decided, agenda issues are ordered by agenda number, not by
	// issue number; D10 sorts after D9.
	s := Snapshot{AgendaPrefix: "D", Issues: []Issue{
		issue(1, KindAgenda, StatusTodo, "D10"),
		issue(2, KindAgenda, StatusTodo, "D9"),
		issue(3, KindHuman, StatusDone, "D11"),
		issue(4, KindAgenda, StatusTodo, "D2"),
		issue(5, KindAgenda, StatusTodo, ""),
		issue(6, KindAgenda, StatusInProgress, "D3", panelEv(markers.PanelStarted, 1)),
	}}
	q := Build(s)
	if got, want := refs(q), []Ref{ref(3), ref(4), ref(6), ref(2), ref(1), ref(5)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if HumanPending(q) {
		t.Errorf("HumanPending with only decided humans")
	}
	s.Issues = append(s.Issues, issue(7, KindHuman, StatusTodo, "D12"))
	q = Build(s)
	if q[0].Ref != ref(3) || q[1].Ref != ref(7) {
		t.Errorf("humans first: %v", refs(q))
	}
	if !HumanPending(q) || !q[1].Human() || q[2].Human() {
		t.Errorf("HumanPending/Human wrong: %v", refs(q))
	}
}

func TestCandidates(t *testing.T) {
	superseded := markers.Superseded{By: string(ref(9))}
	issues := []Issue{
		issue(1, KindAgenda, StatusTodo, "D1"),                                             // ready
		issue(2, KindAgenda, StatusInProgress, "D2", panelEv(markers.PanelStarted, 1)),     // deliberating
		issue(3, KindAgenda, StatusInProgress, "D3", panelEv(markers.PanelInterrupted, 1)), // interrupted
		issue(4, KindAgenda, StatusInProgress, "D4", panelEv(markers.PanelFailed, 1)),      // failed
		issue(5, KindAgenda, StatusInProgress, "D5", panelEv(markers.PanelInterrupted, 1), superseded),
		issue(6, KindAgenda, StatusDone, "D6", panelEv(markers.PanelStarted, 1)), // decided
		blocked(issue(7, KindAgenda, StatusTodo, "D7", panelEv(markers.PanelBlocked, 1)), 8),
		issue(8, KindHuman, StatusDone, "D8"),
		issue(9, KindHuman, StatusDone, "D9"),
		issue(10, KindAgenda, StatusTodo, "D10", panelEv(markers.PanelBlocked, 1)), // prerequisite decided: ready
	}
	type c = Candidate
	for _, tc := range []struct {
		name   string
		issues []Issue
		live   map[Ref]bool
		full   bool
		want   []Candidate
	}{
		{"event", issues, nil, false, []Candidate{
			c{ref(1), StartConvene}, c{ref(3), StartResume}, c{ref(7), StartConvene}, c{ref(10), StartConvene},
		}},
		{"full rebuild adds failed and orphaned deliberating", issues, nil, true, []Candidate{
			c{ref(1), StartConvene}, c{ref(2), StartResume}, c{ref(3), StartResume}, c{ref(4), StartResume},
			c{ref(7), StartConvene}, c{ref(10), StartConvene},
		}},
		{"live panels are skipped", issues, map[Ref]bool{ref(1): true, ref(2): true, ref(3): true}, true, []Candidate{
			c{ref(4), StartResume}, c{ref(7), StartConvene}, c{ref(10), StartConvene},
		}},
		{"undecided human holds every agenda issue", append(slices.Clone(issues), issue(11, KindHuman, StatusTodo, "D11")), nil, true, []Candidate{
			c{ref(11), StartConvene},
		}},
		{"interrupted human issue resumes", []Issue{
			issue(12, KindHuman, StatusInProgress, "D12", panelEv(markers.PanelInterrupted, 1)),
			issue(13, KindAgenda, StatusTodo, "D1"),
		}, nil, false, []Candidate{c{ref(12), StartResume}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Candidates(Build(Snapshot{AgendaPrefix: "D", Issues: tc.issues}), tc.live, tc.full)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Candidates = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgendaNumber(t *testing.T) {
	for in, want := range map[string]int{"D1": 1, "D25": 25, "D": 0, "D0": 0, "D01": 0, "X3": 0, "": 0, "D-1": 0} {
		if got := AgendaNumber("D", in); got != want {
			t.Errorf("AgendaNumber(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseRef(t *testing.T) {
	if r, n, ok := ParseRef("o/r#12"); !ok || r != "o/r" || n != 12 {
		t.Errorf("ParseRef = %q %d %v", r, n, ok)
	}
	for _, bad := range []string{"o/r", "o#1", "o/r#x", "o/r#0", "/r#1", "a/b/c#1"} {
		if _, _, ok := ParseRef(bad); ok {
			t.Errorf("ParseRef(%q) ok", bad)
		}
	}
}
