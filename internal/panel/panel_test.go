package panel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/oh-my-omp/pm-council/internal/markers"
)

func TestEndMarker(t *testing.T) {
	killed := []markers.PanelSession{{Seat: "Argon", Kind: "vote", Session: "s1", File: "/x/s1.jsonl"}}
	reason := "omp exited before a goal tool [pm_vote] succeeded: boom"
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
		{"daemon stopping posts nothing", context.Canceled, killed, markers.Panel{}, false},
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
		{"draft below the round does not belong to it", []markers.Marker{pn(markers.PanelStarted, 1), d1, pn(markers.PanelResumed, 2)},
			Step{Kind: StepDraft, Round: 2, Version: 2}},
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
