package markers

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, m := range []Marker{
		Panel{Event: PanelInterrupted, Round: 2, Sessions: []PanelSession{{Seat: "Argon", Kind: "vote", Session: "s1", File: "/x/s1.jsonl"}}, Prereq: nil, Error: nil},
		Panel{Event: PanelBlocked, Round: 1, Sessions: []PanelSession{}, Prereq: new("o/r#3"), Error: nil},
		Draft{Version: 1, Rows: []Row{{ID: "D1-01", What: "w", Where: "x", How: "h", Tradeoff: "t", Category: "c", MissingDesign: true, Deliver: &Deliver{Repo: "o/c", Title: "t", Body: "b"}}}, Supersedes: []string{}, Settle: &Settle{Kind: SettleReplace, Text: "x"}, Withdrawn: nil},
		Vote{Seat: "ClaudeHigh", Version: 1, Rows: []VoteRow{{Row: "D1-01", Choice: ChoiceOppose, Counterexample: new("c"), Citation: nil, Classification: nil}}},
		Ruling{Seat: "LeadMentor", Version: 1, Rows: []RulingRow{{Row: "D1-01", Category: "c"}}},
		Decision{Version: 1, Rows: []DecisionRow{{Row: "D1-01", Outcome: OutcomePassed, Approve: 3, Threshold: 3, Reason: "r"}}, Impl: []string{"o/c#1"}, Superseded: []string{}},
		Superseded{By: "o/r#9"},
		Escalation{Issue: "o/c#3", Ask: 6695362696},
	} {
		line, err := Format(m)
		if err != nil {
			t.Fatalf("format %T: %v", m, err)
		}
		if !strings.HasPrefix(line, "<!-- meerkat:"+string(m.Kind())+` {"v":1,`) || !strings.HasSuffix(line, " -->") {
			t.Fatalf("format %T = %s", m, line)
		}
		got, err := Parse("Human text.\n\n" + line + "\n")
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("round trip %T:\n got %#v\nwant %#v", m, got, m)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"missing field", `<!-- meerkat:panel {"v":1,"event":"started","round":1,"sessions":[],"prereq":null} -->`, "error: missing"},
		{"missing nested field", `<!-- meerkat:vote {"v":1,"seat":"A","version":1,"rows":[{"row":"D1-01","choice":"approve","citation":null,"classification":null}]} -->`, "rows[0].counterexample: missing"},
		{"null non-pointer", `<!-- meerkat:superseded {"v":1,"by":null} -->`, "by: null"},
		{"wrong type", `<!-- meerkat:panel {"v":1,"event":"started","round":"1","sessions":[],"prereq":null,"error":null} -->`, "round"},
		{"bad enum", `<!-- meerkat:panel {"v":1,"event":"paused","round":1,"sessions":[],"prereq":null,"error":null} -->`, "event"},
		{"missing v", `<!-- meerkat:superseded {"by":"o/r#1"} -->`, "v: missing"},
		{"wrong v", `<!-- meerkat:superseded {"v":2,"by":"o/r#1"} -->`, "v: want 1"},
		{"unknown kind", `<!-- meerkat:impl {"v":1} -->`, "not a council marker kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.body)
			if err == nil || errors.Is(err, ErrNoMarker) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseNoMarker(t *testing.T) {
	for _, body := range []string{
		"",
		"plain comment",
		"<!-- meerkat:panel {\"v\":1} -->\nmarker not on the last line",
	} {
		if _, err := Parse(body); !errors.Is(err, ErrNoMarker) {
			t.Errorf("Parse(%q) err = %v, want ErrNoMarker", body, err)
		}
	}
}

func TestParseHandWritten(t *testing.T) {
	m, err := Parse(`<!-- meerkat:panel {"v":1,"event":"started","round":1,"sessions":[],"prereq":null,"error":null} -->`)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := m.(Panel); !ok || p.Event != PanelStarted || p.Round != 1 {
		t.Fatalf("got %#v", m)
	}
}

func TestParseLineMatchesParse(t *testing.T) {
	bodies := []string{
		"hello\n\n<!-- meerkat:superseded {\"v\":1,\"by\":\"o/agenda#9\"} -->\n",
		"pad\r\n<!-- meerkat:panel {\"v\":1,\"event\":\"started\",\"round\":1,\"sessions\":[],\"prereq\":null,\"error\":null} -->  \t\r\n",
		"a\nb\n<!-- meerkat:vote {\"v\":1,\"seat\":\"A\",\"version\":1,\"rows\":[{\"row\":\"D1-01\",\"choice\":\"approve\",\"counterexample\":null,\"citation\":null,\"classification\":null}]} -->",
		"plain comment",
		"",
		"<!-- meerkat:panel {\"v\":1} -->\nmarker not on the last line",
	}
	for _, body := range bodies {
		want, wantErr := Parse(body)
		got, gotErr := ParseLine(LastLine(body))
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("body %q: Parse err %v, ParseLine err %v", body, wantErr, gotErr)
		}
		if wantErr != nil {
			if wantErr.Error() != gotErr.Error() && !errors.Is(wantErr, ErrNoMarker) {
				t.Fatalf("body %q: Parse err %q, ParseLine err %q", body, wantErr, gotErr)
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("body %q:\n got %#v\nwant %#v", body, got, want)
		}
	}
}
