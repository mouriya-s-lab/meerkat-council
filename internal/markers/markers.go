// Package markers parses and formats PM machine markers: the last line of an issue or comment
// body, `<!-- pm:<kind> <JSON> -->`, with single-line JSON carrying "v":1 (contract §2, §A, §B).
// A marker whose JSON lacks a field of its kind, has a field of the wrong type or an unknown enum
// value, or whose "v" is not 1 does not count as a marker.
package markers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Version is the "v" every marker carries.
const Version = 1

// Kind names a marker kind.
type Kind string

const (
	KindPanel      Kind = "panel"
	KindDraft      Kind = "draft"
	KindVote       Kind = "vote"
	KindRuling     Kind = "ruling"
	KindDecision   Kind = "decision"
	KindSuperseded Kind = "superseded"
	KindEscalation Kind = "escalation"
	KindReply      Kind = "reply"
	KindReplaced   Kind = "replaced"
	KindReplaces   Kind = "replaces"
)

// Marker is one of Panel, Draft, Vote, Ruling, Decision, Superseded, Escalation (agenda issues)
// or Reply, Replaced, Replaces (implementation issues, contract §2).
type Marker interface {
	Kind() Kind
	validate() error
}

// PanelEvent is a panel state change.
type PanelEvent string

const (
	PanelStarted     PanelEvent = "started"
	PanelResumed     PanelEvent = "resumed"
	PanelInterrupted PanelEvent = "interrupted"
	PanelBlocked     PanelEvent = "blocked"
	PanelFailed      PanelEvent = "failed"
)

// Panel records a panel state change on an agenda issue.
type Panel struct {
	Event    PanelEvent     `json:"event"`
	Round    int            `json:"round"`
	Sessions []PanelSession `json:"sessions"`
	Prereq   *string        `json:"prereq"`
	Error    *string        `json:"error"`
}

type PanelSession struct {
	Seat    string `json:"seat"`
	Kind    string `json:"kind"`
	Session string `json:"session"`
	File    string `json:"file"`
}

// Draft is one version of a panel's decision table.
type Draft struct {
	Version    int      `json:"version"`
	Rows       []Row    `json:"rows"`
	Supersedes []string `json:"supersedes"`
	Settle     *Settle  `json:"settle"`
	Withdrawn  *string  `json:"withdrawn"`
}

type Row struct {
	ID            string   `json:"id"`
	What          string   `json:"what"`
	Where         string   `json:"where"`
	How           string   `json:"how"`
	Tradeoff      string   `json:"tradeoff"`
	Category      string   `json:"category"`
	MissingDesign bool     `json:"missingDesign"`
	Deliver       *Deliver `json:"deliver"`
}

type Deliver struct {
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// SettleKind is how an escalated ask is settled.
type SettleKind string

const (
	SettleReply   SettleKind = "reply"
	SettleReplace SettleKind = "replace"
)

type Settle struct {
	Kind SettleKind `json:"kind"`
	Text string     `json:"text"`
}

// Vote is one seat's vote on one draft version.
type Vote struct {
	Seat    string    `json:"seat"`
	Version int       `json:"version"`
	Rows    []VoteRow `json:"rows"`
}

// Choice is a seat's choice on one row.
type Choice string

const (
	ChoiceApprove Choice = "approve"
	ChoiceOppose  Choice = "oppose"
	ChoiceVeto    Choice = "veto"
)

type VoteRow struct {
	Row            string  `json:"row"`
	Choice         Choice  `json:"choice"`
	Counterexample *string `json:"counterexample"`
	Citation       *string `json:"citation"`
	Classification *string `json:"classification"`
}

// Ruling is an arbitrating seat's categorisation of rows.
type Ruling struct {
	Seat    string      `json:"seat"`
	Version int         `json:"version"`
	Rows    []RulingRow `json:"rows"`
}

type RulingRow struct {
	Row      string `json:"row"`
	Category string `json:"category"`
}

// Decision is the tally of a draft version.
type Decision struct {
	Version    int           `json:"version"`
	Rows       []DecisionRow `json:"rows"`
	Impl       []string      `json:"impl"`
	Superseded []string      `json:"superseded"`
}

// Outcome is a row's result.
type Outcome string

const (
	OutcomePassed    Outcome = "passed"
	OutcomeRejected  Outcome = "rejected"
	OutcomeWithdrawn Outcome = "withdrawn"
)

type DecisionRow struct {
	Row       string  `json:"row"`
	Outcome   Outcome `json:"outcome"`
	Approve   int     `json:"approve"`
	Threshold int     `json:"threshold"`
	Reason    string  `json:"reason"`
}

// Superseded marks an agenda issue changed by a human issue.
type Superseded struct {
	By string `json:"by"`
}

// Escalation is the back-reference in the body of an agenda issue raised from an ask.
type Escalation struct {
	Issue string `json:"issue"`
	Ask   int64  `json:"ask"`
}

// Reply settles an ask on an implementation issue; Decision is the agenda issue when the council
// decided it.
type Reply struct {
	Ask      int64   `json:"ask"`
	Decision *string `json:"decision"`
}

// Replaced marks an implementation issue replaced by another, settling Ask when not nil.
type Replaced struct {
	Ask *int64 `json:"ask"`
	By  string `json:"by"`
}

// Replaces ends the body of an implementation issue that replaces Old.
type Replaces struct {
	Old string `json:"old"`
	Ask *int64 `json:"ask"`
}

func (Panel) Kind() Kind      { return KindPanel }
func (Draft) Kind() Kind      { return KindDraft }
func (Vote) Kind() Kind       { return KindVote }
func (Ruling) Kind() Kind     { return KindRuling }
func (Decision) Kind() Kind   { return KindDecision }
func (Superseded) Kind() Kind { return KindSuperseded }
func (Escalation) Kind() Kind { return KindEscalation }
func (Reply) Kind() Kind      { return KindReply }
func (Replaced) Kind() Kind   { return KindReplaced }
func (Replaces) Kind() Kind   { return KindReplaces }

func (p Panel) validate() error {
	switch p.Event {
	case PanelStarted, PanelResumed, PanelInterrupted, PanelBlocked, PanelFailed:
		return nil
	}
	return fmt.Errorf("event: unknown value %q", p.Event)
}

func (d Draft) validate() error {
	if d.Settle != nil {
		switch d.Settle.Kind {
		case SettleReply, SettleReplace:
		default:
			return fmt.Errorf("settle.kind: unknown value %q", d.Settle.Kind)
		}
	}
	return nil
}

func (v Vote) validate() error {
	for i, r := range v.Rows {
		switch r.Choice {
		case ChoiceApprove, ChoiceOppose, ChoiceVeto:
		default:
			return fmt.Errorf("rows[%d].choice: unknown value %q", i, r.Choice)
		}
	}
	return nil
}

func (Ruling) validate() error { return nil }

func (d Decision) validate() error {
	for i, r := range d.Rows {
		switch r.Outcome {
		case OutcomePassed, OutcomeRejected, OutcomeWithdrawn:
		default:
			return fmt.Errorf("rows[%d].outcome: unknown value %q", i, r.Outcome)
		}
	}
	return nil
}

func (Superseded) validate() error { return nil }
func (Escalation) validate() error { return nil }
func (Reply) validate() error      { return nil }
func (Replaced) validate() error   { return nil }
func (Replaces) validate() error   { return nil }

var linePattern = regexp.MustCompile(`^<!-- pm:([a-z]+) (\{.*\}) -->$`)

// ErrNoMarker means the body's last line is not a pm marker line.
var ErrNoMarker = errors.New("no pm marker")

// Parse returns the marker on the last non-blank line of body. It returns ErrNoMarker when that
// line is not `<!-- pm:<kind> <JSON> -->`, and another error when it is one but does not parse
// into a council marker of that kind.
func Parse(body string) (Marker, error) {
	body = strings.TrimRight(body, " \t\r\n")
	line := body[strings.LastIndexByte(body, '\n')+1:]
	m := linePattern.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return nil, ErrNoMarker
	}
	kind, raw := Kind(m[1]), []byte(m[2])
	var mk Marker
	switch kind {
	case KindPanel:
		mk = &Panel{}
	case KindDraft:
		mk = &Draft{}
	case KindVote:
		mk = &Vote{}
	case KindRuling:
		mk = &Ruling{}
	case KindDecision:
		mk = &Decision{}
	case KindSuperseded:
		mk = &Superseded{}
	case KindEscalation:
		mk = &Escalation{}
	case KindReply:
		mk = &Reply{}
	case KindReplaced:
		mk = &Replaced{}
	case KindReplaces:
		mk = &Replaces{}
	default:
		return nil, fmt.Errorf("pm:%s: not a council marker kind", kind)
	}
	if err := decode(raw, mk); err != nil {
		return nil, fmt.Errorf("pm:%s: %w", kind, err)
	}
	// Return the value, not the pointer, so callers switch on value types.
	return reflect.ValueOf(mk).Elem().Interface().(Marker), nil
}

func decode(raw []byte, dst Marker) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	var v int
	if r, ok := obj["v"]; !ok {
		return errors.New("v: missing")
	} else if err := json.Unmarshal(r, &v); err != nil || v != Version {
		return fmt.Errorf("v: want %d, got %s", Version, r)
	}
	if err := present(reflect.TypeOf(dst).Elem(), raw, ""); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return err
	}
	return dst.validate()
}

// present checks that raw has every json-tagged field of t, recursively, and that only pointer
// fields hold null.
func present(t reflect.Type, raw json.RawMessage, path string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if t.Kind() == reflect.Pointer {
			return nil
		}
		return fmt.Errorf("%s: null", label(path))
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("%s: %w", label(path), err)
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			fp := name
			if path != "" {
				fp = path + "." + name
			}
			r, ok := obj[name]
			if !ok {
				return fmt.Errorf("%s: missing", fp)
			}
			if err := present(f.Type, r, fp); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("%s: %w", label(path), err)
		}
		for i, r := range items {
			if err := present(t.Elem(), r, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func label(path string) string {
	if path == "" {
		return "marker"
	}
	return path
}

// Format renders m as its marker line, `<!-- pm:<kind> {"v":1,...} -->`.
func Format(m Marker) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(b) < 2 || b[0] != '{' {
		return "", fmt.Errorf("pm:%s: marker is not a JSON object", m.Kind())
	}
	sep := ","
	if string(b) == "{}" {
		sep = ""
	}
	return fmt.Sprintf(`<!-- pm:%s {"v":%d%s%s -->`, m.Kind(), Version, sep, b[1:]), nil
}

// Comment renders a comment body: text, a blank line, then m's marker line. It fails when the line
// would not parse back as a marker (a missing field or unknown enum value).
func Comment(text string, m Marker) (string, error) {
	line, err := Format(m)
	if err != nil {
		return "", err
	}
	if _, err := Parse(line); err != nil {
		return "", err
	}
	return strings.TrimRight(text, "\n") + "\n\n" + line, nil
}

// Collect parses the marker of each body, oldest first. Bodies without a marker are skipped;
// malformed markers are skipped and returned as errors.
func Collect(bodies []string) ([]Marker, []error) {
	var ms []Marker
	var errs []error
	for _, b := range bodies {
		m, err := Parse(b)
		switch {
		case errors.Is(err, ErrNoMarker):
		case err != nil:
			errs = append(errs, err)
		default:
			ms = append(ms, m)
		}
	}
	return ms, errs
}

// LastPanel returns the latest pm:panel marker of ms.
func LastPanel(ms []Marker) (Panel, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if p, ok := ms[i].(Panel); ok {
			return p, true
		}
	}
	return Panel{}, false
}

// LastDecision returns the latest pm:decision marker of ms.
func LastDecision(ms []Marker) (Decision, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if d, ok := ms[i].(Decision); ok {
			return d, true
		}
	}
	return Decision{}, false
}
