package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mouriya-s-lab/meerkat-council/internal/config"
	"github.com/mouriya-s-lab/meerkat-council/internal/github"
	"github.com/mouriya-s-lab/meerkat-council/internal/markers"
	"github.com/mouriya-s-lab/meerkat-council/internal/queue"
	"github.com/mouriya-s-lab/meerkat-council/internal/sessions"
	"github.com/mouriya-s-lab/meerkat-council/internal/tally"
)

func TestEndMarker(t *testing.T) {
	killed := []markers.PanelSession{{Seat: "Argon", Kind: "vote", Session: "s1", File: "/x/s1.jsonl"}}
	reason := "omp exited before a goal tool [meerkat_vote] succeeded: boom"
	for _, tc := range []struct {
		name  string
		cause error
		open  []markers.PanelSession
		want  markers.Panel
		post  bool
	}{
		{"interrupted lists the killed sessions", fmt.Errorf("wrapped: %w", ErrInterrupted), killed,
			markers.Panel{Event: markers.PanelInterrupted, Round: 2, Sessions: killed}, true},
		{"interrupted with none killed", ErrInterrupted, nil,
			markers.Panel{Event: markers.PanelInterrupted, Round: 2, Sessions: []markers.PanelSession{}}, true},
		{"failed carries the error and no sessions", nil, killed,
			markers.Panel{Event: markers.PanelFailed, Round: 2, Sessions: []markers.PanelSession{}, Error: &reason}, true},
		{"daemon stopping posts nothing", sessions.Shutdown(time.Now()), killed, markers.Panel{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, post := endMarker(tc.cause, errors.New(reason), 2, tc.open)
			if post != tc.post || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("endMarker = %+v %v, want %+v %v", got, post, tc.want, tc.post)
			}
		})
	}
}

func TestDerive(t *testing.T) {
	voters := []string{"Coordination", "ClaudeHigh", "Argon"}
	pn := func(ev markers.PanelEvent, round int) markers.Panel {
		return markers.Panel{Event: ev, Round: round, Sessions: []markers.PanelSession{}}
	}
	draft := func(v int) markers.Draft {
		return markers.Draft{Version: v, Rows: []markers.Row{{ID: "D1-01"}}, Supersedes: []string{}}
	}
	vote := func(seat string, v int) markers.Vote {
		return markers.Vote{Seat: seat, Version: v, Rows: []markers.VoteRow{}}
	}
	d1, d2 := draft(1), draft(2)
	withdrawn := draft(2)
	withdrawn.Withdrawn = new("dropped")
	for _, tc := range []struct {
		name string
		ms   []markers.Marker
		want Step
	}{
		{"no markers: draft v1 of round 1", nil, Step{Kind: StepDraft, Round: 1, Version: 1}},
		{"started: draft v1", []markers.Marker{pn(markers.PanelStarted, 1)}, Step{Kind: StepDraft, Round: 1, Version: 1}},
		{"draft v1: every voter pending", []markers.Marker{pn(markers.PanelStarted, 1), d1},
			Step{Kind: StepVote, Round: 1, Version: 1, Draft: &d1, Pending: voters}},
		{"some voted; duplicate votes count once", []markers.Marker{
			pn(markers.PanelStarted, 1), d1, vote("Argon", 1), vote("Argon", 1),
			pn(markers.PanelInterrupted, 1), pn(markers.PanelResumed, 1), vote("Coordination", 1),
		}, Step{Kind: StepVote, Round: 1, Version: 1, Draft: &d1, Pending: []string{"ClaudeHigh"}}},
		{"all voted: done", []markers.Marker{
			pn(markers.PanelStarted, 1), d1, vote("Argon", 1), vote("ClaudeHigh", 1), vote("Coordination", 1),
		}, Step{Kind: StepDone, Round: 1, Version: 1, Draft: &d1}},
		{"votes on another version do not count", []markers.Marker{
			pn(markers.PanelStarted, 2), d1, d2, vote("Argon", 1), vote("ClaudeHigh", 2),
		}, Step{Kind: StepVote, Round: 2, Version: 2, Draft: &d2, Pending: []string{"Coordination", "Argon"}}},
		{"a later round revises the draft below it", []markers.Marker{pn(markers.PanelStarted, 1), d1, pn(markers.PanelStarted, 2)},
			Step{Kind: StepDraft, Round: 2, Version: 2, Draft: &d1}},
		{"a withdrawn draft is not voted on", []markers.Marker{pn(markers.PanelStarted, 2), d1, withdrawn},
			Step{Kind: StepDone, Round: 2, Version: 2, Draft: &withdrawn}},
		{"failed during voting resumes the vote step", []markers.Marker{
			pn(markers.PanelStarted, 1), d1, vote("Argon", 1), pn(markers.PanelFailed, 1),
		}, Step{Kind: StepVote, Round: 1, Version: 1, Draft: &d1, Pending: []string{"Coordination", "ClaudeHigh"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Derive(tc.ms, voters); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Derive = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRulingCovers(t *testing.T) {
	disputes := []tally.Dispute{{Row: markers.Row{ID: "D1-01"}}, {Row: markers.Row{ID: "D1-02"}}}
	cats := []string{"a", "b"}
	rr := func(row, cat string) markers.RulingRow { return markers.RulingRow{Row: row, Category: cat} }
	for _, tc := range []struct {
		name string
		rows []markers.RulingRow
		ok   bool
	}{
		{"every disputed row once", []markers.RulingRow{rr("D1-02", "b"), rr("D1-01", "a")}, true},
		{"a row missing", []markers.RulingRow{rr("D1-01", "a")}, false},
		{"a row twice", []markers.RulingRow{rr("D1-01", "a"), rr("D1-01", "b"), rr("D1-02", "a")}, false},
		{"an undisputed row", []markers.RulingRow{rr("D1-01", "a"), rr("D1-02", "a"), rr("D1-03", "a")}, false},
		{"an unknown category", []markers.RulingRow{rr("D1-01", "a"), rr("D1-02", "z")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := rulingCovers(tc.rows, disputes, cats); (err == nil) != tc.ok {
				t.Errorf("rulingCovers = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

func TestForkFile(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(existing, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone.jsonl")
	r := &run{Runner: &Runner{}, job: Job{Ref: queue.Ref("o/r#7")}}
	for _, tc := range []struct {
		name string
		fork *markers.PanelSession
		want string
	}{
		{"nil fork starts fresh", nil, ""},
		{"empty file starts fresh", &markers.PanelSession{Seat: "Argon", Kind: "vote"}, ""},
		{"missing file starts fresh", &markers.PanelSession{Seat: "Argon", Kind: "vote", File: missing}, ""},
		{"existing file forks", &markers.PanelSession{Seat: "Argon", Kind: "vote", File: existing}, existing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.forkFile(config.KindVote, "Argon", tc.fork); got != tc.want {
				t.Errorf("forkFile = %q, want %q", got, tc.want)
			}
		})
	}
}

// omittedDeliverArgs is what a real draft session sends: rows whose deliver key is absent.
const omittedDeliverArgs = `{"rows":[{"id":"S1-01","what":"决策:结论","where":"X.md","how":"读X后给结论","tradeoff":"无","category":"意图","missingDesign":false}],"supersedes":[],"settle":null,"withdrawn":null}`

func TestNormalizeDeliver(t *testing.T) {
	rows, err := normalizeDeliver(json.RawMessage(omittedDeliverArgs))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "S1-01" || rows[0].Deliver != nil {
		t.Fatalf("rows = %+v", rows)
	}
	// The decoded rows must format as a marker whose deliver is an explicit null.
	d := markers.Draft{Version: 1, Rows: rows, Supersedes: []string{}}
	body, err := markers.Comment("text", d)
	if err != nil {
		t.Fatalf("Comment with a deliver-less row: %v", err)
	}
	if !strings.Contains(body, `"deliver":null`) {
		t.Errorf("marker body %q lacks a null deliver", body)
	}
	if _, err := markers.Parse(body); err != nil {
		t.Errorf("marker body does not parse back: %v", err)
	}
	// An explicit null deliver stays null; an object deliver keeps the object.
	for _, tc := range []struct {
		name        string
		raw         string
		wantDeliver bool // want a non-nil Deliver
	}{
		{"explicit null", `{"rows":[{"id":"S1-01","what":"w","where":"x","how":"h","tradeoff":"t","category":"意图","missingDesign":false,"deliver":null}],"supersedes":[],"settle":null,"withdrawn":null}`, false},
		{"object", `{"rows":[{"id":"S1-01","what":"w","where":"x","how":"h","tradeoff":"t","category":"意图","missingDesign":false,"deliver":{"repo":"o/c","title":"t","body":"b"}}],"supersedes":[],"settle":null,"withdrawn":null}`, true},
	} {
		rows, err := normalizeDeliver(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (rows[0].Deliver != nil) != tc.wantDeliver {
			t.Errorf("%s: deliver = %+v, want non-nil %t", tc.name, rows[0].Deliver, tc.wantDeliver)
		}
	}
}

// draftGitHub is an issue with fixed comments that records the comments a panel posts.
type draftGitHub struct {
	comments []string
	posted   []string
}

func (g *draftGitHub) Issue(context.Context, string, int) (github.Issue, error) {
	return github.Issue{Title: "t"}, nil
}
func (g *draftGitHub) Comment(context.Context, string, int64) (string, error) { return "", nil }
func (g *draftGitHub) EachComment(_ context.Context, _ string, _ int, fn func(string, time.Time) error) error {
	for _, c := range g.comments {
		if err := fn(c, time.Time{}); err != nil {
			return err
		}
	}
	return nil
}
func (g *draftGitHub) PostComment(_ context.Context, _ string, _ int, body string) (int64, error) {
	g.posted = append(g.posted, body)
	return int64(len(g.posted)), nil
}
func (g *draftGitHub) SetOption(context.Context, string, string, string, string) error { return nil }

// TestPostDraftOmittedNullables: meerkat_draft may omit settle and withdrawn (issue #43); the posted
// meerkat:draft is the same as with explicit nulls, and a draft session's withdrawal is refused with
// the plugin's wording.
func TestPostDraftOmittedNullables(t *testing.T) {
	const row = `{"id":"S1-01","what":"w","where":"x","how":"h","tradeoff":"t","category":"意图","missingDesign":false}`
	post := func(t *testing.T, comments []string, st Step, args string) (*draftGitHub, error) {
		t.Helper()
		gh := &draftGitHub{comments: comments}
		p := &run{Runner: &Runner{gh: gh}, job: Job{Ref: queue.Ref("o/agenda#7")}, repo: "o/agenda", number: 7}
		err := p.postDraft(context.Background(), st, sessions.Result{SessionID: "s1", ToolName: config.ToolDraft, Args: json.RawMessage(args)})
		return gh, err
	}
	first := Step{Kind: StepDraft, Round: 1}

	nulls, err := post(t, nil, first, `{"rows":[`+row+`],"supersedes":[],"settle":null,"withdrawn":null}`)
	if err != nil {
		t.Fatal(err)
	}
	omitted, err := post(t, nil, first, `{"rows":[`+row+`],"supersedes":[]}`)
	if err != nil {
		t.Fatalf("draft without settle and withdrawn: %v", err)
	}
	if len(omitted.posted) != 1 || len(nulls.posted) != 1 {
		t.Fatalf("posted %d and %d drafts, want 1 each", len(omitted.posted), len(nulls.posted))
	}
	if omitted.posted[0] != nulls.posted[0] {
		t.Errorf("omitted keys posted\n%s\nexplicit nulls posted\n%s", omitted.posted[0], nulls.posted[0])
	}
	for _, want := range []string{`"settle":null`, `"withdrawn":null`, `"deliver":null`} {
		if !strings.Contains(omitted.posted[0], want) {
			t.Errorf("meerkat:draft %q lacks %s", omitted.posted[0], want)
		}
	}
	m, err := markers.Parse(omitted.posted[0])
	if err != nil {
		t.Fatalf("meerkat:draft does not parse back: %v", err)
	}
	if d, ok := m.(markers.Draft); !ok || d.Version != 1 || d.Settle != nil || d.Withdrawn != nil {
		t.Errorf("meerkat:draft = %#v", m)
	}

	gh, err := post(t, nil, first, `{"rows":[`+row+`],"supersedes":[],"withdrawn":"撤回"}`)
	if err == nil || !strings.Contains(err.Error(), "withdrawn must be null: only a revise session may withdraw the agenda") {
		t.Errorf("draft session withdrawing: err = %v", err)
	}
	if len(gh.posted) != 0 {
		t.Errorf("a refused withdrawal posted %q", gh.posted)
	}

	// A revise session that omits withdrawn posts the next version, not withdrawn.
	d1 := markers.Draft{Version: 1, Rows: []markers.Row{{ID: "S1-01", What: "w", Where: "x", How: "h", Tradeoff: "t", Category: "意图"}}, Supersedes: []string{}}
	c1, err := markers.Comment("v1", d1)
	if err != nil {
		t.Fatal(err)
	}
	gh, err = post(t, []string{c1}, Step{Kind: StepDraft, Round: 2, Version: 1, Draft: &d1}, `{"rows":[`+row+`],"supersedes":[]}`)
	if err != nil {
		t.Fatalf("revise without withdrawn: %v", err)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("revise posted %d drafts, want 1", len(gh.posted))
	}
	if m, err := markers.Parse(gh.posted[0]); err != nil {
		t.Fatal(err)
	} else if d, ok := m.(markers.Draft); !ok || d.Version != 2 || d.Withdrawn != nil {
		t.Errorf("revised meerkat:draft = %#v", m)
	}
}
