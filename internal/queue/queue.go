// Package queue derives the agenda queue from a snapshot of the PM repository: which agenda
// issues exist, which are human, which have a panel, which wait on an undecided human
// prerequisite, which are decided or superseded (contract §1, §A, §C; box
// pm_councilProgram_queue). Pure functions; no IO.
package queue

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oh-my-omp/pm-council/internal/markers"
)

// Ref is an issue reference, "owner/repo#n".
type Ref string

// FormatRef builds the Ref of issue number n in repo "owner/name".
func FormatRef(repo string, n int) Ref { return Ref(fmt.Sprintf("%s#%d", repo, n)) }

// ParseRef splits "owner/repo#n" into its repository and number.
func ParseRef(s string) (repo string, n int, ok bool) {
	repo, num, ok := strings.Cut(s, "#")
	if !ok || strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return repo, n, true
}

// Kind is the semantic Project Kind of an issue.
type Kind int

const (
	KindNone Kind = iota // no Kind set: not on the agenda
	KindHuman
	KindAgenda
	KindImplementation
)

// Status is the semantic Project Status of an issue.
type Status int

const (
	StatusNone Status = iota // no Status set
	StatusTodo
	StatusInProgress
	StatusDone
	StatusSuperseded
)

// Issue is one PM-repository issue in the Project, as read from GitHub.
type Issue struct {
	Ref    Ref
	Number int
	Closed bool
	Kind   Kind
	Status Status
	// StatusName is the GitHub option name of Status; empty when unset.
	StatusName string
	// Agenda is the Agenda field text; empty when unset.
	Agenda    string
	BlockedBy []Ref
	// Markers are the council markers of the issue's comments, oldest first.
	Markers []markers.Marker
	// PanelAt is the creation time of the comment holding the latest pm:panel marker; zero
	// without one.
	PanelAt time.Time
}

// LeftDeliberation reports whether the issue can no longer be deliberated: it is closed, or its
// Status is the done or superseded option. A live panel of such an issue is stopped.
func (is Issue) LeftDeliberation() bool {
	return is.Closed || is.Status == StatusDone || is.Status == StatusSuperseded
}

// Seat is what the queue needs to know about a council seat.
type Seat struct {
	ID      string
	CanVote bool
}

// Snapshot is the PM repository as read in one rebuild.
type Snapshot struct {
	Issues       []Issue
	Seats        []Seat
	AgendaPrefix string
}

// State is why an agenda issue can or cannot be convened now (§C), in decision order.
type State string

const (
	StateDecided      State = "decided"
	StateSuperseded   State = "superseded"
	StateBlocked      State = "blocked"
	StateHeld         State = "held"
	StateDeliberating State = "deliberating"
	StateInterrupted  State = "interrupted"
	StateFailed       State = "failed"
	StateReady        State = "ready"
)

// Entry is one row of GET /v1/queue.
type Entry struct {
	Ref       Ref      `json:"ref"`
	Agenda    *string  `json:"agenda"`
	Kind      string   `json:"kind"`
	Status    *string  `json:"status"`
	BlockedBy []Ref    `json:"blockedBy"`
	State     State    `json:"state"`
	Notes     []string `json:"notes"`
	Panel     *Panel   `json:"panel"`
	// Closed is whether the issue is closed. A closed issue is never convened or resumed, and a
	// closed human issue neither holds nor blocks another issue; its own state is derived as for
	// an open one.
	Closed bool `json:"closed"`

	human   bool
	number  int // agenda number, 0 when absent or of another prefix
	issue   int
	panelAt time.Time
	// unpublished: decided by a pm:decision whose publish has not reached its last write,
	// Status=Done.
	unpublished bool
}

// Panel is a panel's progress derived from pm:panel, pm:draft and pm:vote markers.
type Panel struct {
	Round   int      `json:"round"`
	Version *int     `json:"version"`
	Votes   []string `json:"votes"`
	Pending []string `json:"pending"`
}

// Queue is the agenda issues in convening order: human issues first, then by agenda number.
type Queue []Entry

// §C kind values.
const (
	kindHuman  = "Human"
	kindAgenda = "Agenda"
)

// Build derives the queue. Issues whose Kind is neither human nor agenda are not on it.
func Build(s Snapshot) Queue {
	byRef := make(map[Ref]*Issue, len(s.Issues))
	for i := range s.Issues {
		byRef[s.Issues[i].Ref] = &s.Issues[i]
	}
	humanPending := false
	for _, is := range s.Issues {
		if is.Kind == KindHuman && !decided(is) && !is.Closed {
			humanPending = true
		}
	}
	q := Queue{}
	for _, is := range s.Issues {
		if is.Kind != KindHuman && is.Kind != KindAgenda {
			continue
		}
		e := Entry{
			Ref:       is.Ref,
			Kind:      kindAgenda,
			BlockedBy: []Ref{},
			Notes:     []string{},
			human:     is.Kind == KindHuman,
			number:    AgendaNumber(s.AgendaPrefix, is.Agenda),
			issue:     is.Number,
			panelAt:   is.PanelAt,
			Closed:    is.Closed,
		}
		if e.human {
			e.Kind = kindHuman
		}
		if is.Agenda != "" {
			e.Agenda = new(is.Agenda)
		}
		if is.StatusName != "" {
			e.Status = new(is.StatusName)
		}
		waiting := false
		for _, p := range is.BlockedBy {
			pre, ok := byRef[p]
			if !ok || pre.Kind != KindHuman {
				e.Notes = append(e.Notes, fmt.Sprintf("Blocked by %s ignored: not a human issue in the Project", p))
				continue
			}
			e.BlockedBy = append(e.BlockedBy, p)
			// A closed prerequisite counts as resolved, as GitHub treats a closed blocker.
			if !pre.Closed && !decided(*pre) {
				waiting = true
			}
		}
		e.Panel = panel(is.Markers, s.Seats)
		e.State = state(is, waiting, humanPending)
		e.unpublished = hasKind(is.Markers, markers.KindDecision) && is.Status != StatusDone
		q = append(q, e)
	}
	slices.SortStableFunc(q, compare)
	return q
}

func state(is Issue, waiting, humanPending bool) State {
	switch {
	case decided(is):
		return StateDecided
	case superseded(is):
		return StateSuperseded
	case waiting:
		return StateBlocked
	case is.Kind != KindHuman && humanPending:
		return StateHeld
	}
	if p, ok := markers.LastPanel(is.Markers); ok {
		switch p.Event {
		case markers.PanelStarted, markers.PanelResumed:
			return StateDeliberating
		case markers.PanelInterrupted:
			return StateInterrupted
		case markers.PanelFailed:
			return StateFailed
		case markers.PanelBlocked:
			// The block itself is a Blocked by relation; once its prerequisite is decided the
			// issue can be convened again.
		}
	}
	return StateReady
}

func decided(is Issue) bool {
	return is.Status == StatusDone || hasKind(is.Markers, markers.KindDecision)
}

func superseded(is Issue) bool {
	return is.Status == StatusSuperseded || hasKind(is.Markers, markers.KindSuperseded)
}

func hasKind(ms []markers.Marker, k markers.Kind) bool {
	return slices.ContainsFunc(ms, func(m markers.Marker) bool { return m.Kind() == k })
}

// panel derives the panel progress: round of the latest pm:panel, the highest draft version,
// the seats that voted on it, and the voting seats that have not. Nil without a pm:panel.
func panel(ms []markers.Marker, seats []Seat) *Panel {
	last, ok := markers.LastPanel(ms)
	if !ok {
		return nil
	}
	p := &Panel{Round: last.Round, Votes: []string{}, Pending: []string{}}
	for _, m := range ms {
		if d, ok := m.(markers.Draft); ok && (p.Version == nil || d.Version > *p.Version) {
			p.Version = new(d.Version)
		}
	}
	voted := map[string]bool{}
	if p.Version != nil {
		for _, m := range ms {
			if v, ok := m.(markers.Vote); ok && v.Version == *p.Version && !voted[v.Seat] {
				voted[v.Seat] = true
				p.Votes = append(p.Votes, v.Seat)
			}
		}
	}
	for _, s := range seats {
		if s.CanVote && !voted[s.ID] {
			p.Pending = append(p.Pending, s.ID)
		}
	}
	return p
}

// compare orders human issues first, then by agenda number (issues without one last), then by
// issue number. All entries are issues of the one PM repository.
func compare(a, b Entry) int {
	if a.human != b.human {
		if a.human {
			return -1
		}
		return 1
	}
	if (a.number == 0) != (b.number == 0) {
		if a.number == 0 {
			return 1
		}
		return -1
	}
	if c := cmp.Compare(a.number, b.number); c != 0 {
		return c
	}
	return cmp.Compare(a.issue, b.issue)
}

// AgendaNumber parses "<prefix><n>" and returns n, or 0 when agenda does not have that form.
func AgendaNumber(prefix, agenda string) int {
	rest, ok := strings.CutPrefix(agenda, prefix)
	if !ok || rest == "" || rest[0] == '0' {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Human reports whether the entry is a human issue.
func (e Entry) Human() bool { return e.human }

// HumanPending reports whether some open human issue on the queue is not decided.
func HumanPending(q Queue) bool {
	return slices.ContainsFunc(q, func(e Entry) bool { return e.human && !e.Closed && e.State != StateDecided })
}

// Start is how a panel is started for an issue.
type Start int

const (
	StartConvene Start = iota // a fresh panel
	StartResume               // continue from the issue's markers
)

func (s Start) String() string {
	switch s {
	case StartConvene:
		return "convene"
	case StartResume:
		return "resume"
	}
	return fmt.Sprintf("Start(%d)", int(s))
}

// Candidate is an issue a panel may be started for now.
type Candidate struct {
	Ref   Ref
	Start Start
}

// Candidates returns, in queue order, the open issues without a live panel that a panel may be
// started for: ready ones are convened; interrupted ones are resumed; failed ones, and
// deliberating ones without a live panel, are resumed only when their latest pm:panel comment was
// created before resumeBefore, so a panel that failed or was left in this daemon process is never
// restarted by it; decided ones whose decision is not fully published are resumed on every full
// rebuild. The daemon passes its start time on full rebuilds and the zero time, which resumes none
// of them, on single-issue refreshes. Closed, superseded, held and blocked issues are never
// candidates.
func Candidates(q Queue, live map[Ref]bool, resumeBefore time.Time) []Candidate {
	var out []Candidate
	for _, e := range q {
		if live[e.Ref] || e.Closed {
			continue
		}
		switch e.State {
		case StateReady:
			out = append(out, Candidate{e.Ref, StartConvene})
		case StateInterrupted:
			out = append(out, Candidate{e.Ref, StartResume})
		case StateFailed, StateDeliberating:
			if e.panelAt.Before(resumeBefore) {
				out = append(out, Candidate{e.Ref, StartResume})
			}
		case StateDecided:
			if e.unpublished && !resumeBefore.IsZero() {
				out = append(out, Candidate{e.Ref, StartResume})
			}
		case StateSuperseded, StateBlocked, StateHeld:
		default:
			panic(fmt.Sprintf("unhandled queue state %q", e.State))
		}
	}
	return out
}
