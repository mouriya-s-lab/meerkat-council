// Package config loads and validates the council configuration under <config-root>:
// council/{runtime,council,seats,goals,proxy-readers}.yaml, the seat prompt files and
// shared/{repos,project}.yaml. Unknown keys and missing required keys are errors that name the
// file and the YAML key path.
package config

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"

	"github.com/oh-my-omp/pm-council/internal/markers"
)

// Config is the validated configuration.
type Config struct {
	Runtime      Runtime
	Council      Council
	Tally        Tally
	Seats        []Seat
	Goals        Goals
	ProxyReaders []ProxyReader
	Repos        Repos
	Project      Project
	Loaded       Loaded
}

// Issue is one configuration error: its file, the YAML key path (empty when the error is not about a
// key) and its message. Its text is the startup error.
type Issue struct {
	File    string `json:"file"`
	Key     string `json:"key"`
	Message string `json:"message"`
}

func (i Issue) Error() string {
	if i.Key == "" {
		return i.File + ": " + i.Message
	}
	return i.File + ": " + i.Key + ": " + i.Message
}

// Loaded is the configuration as it was loaded: when, and the git blob sha of every file read to
// build it, sorted by path.
type Loaded struct {
	LoadedAt time.Time    `json:"loadedAt"`
	Files    []LoadedFile `json:"files"`
}

// LoadedFile is one loaded file: its path relative to the config root and its git blob sha.
type LoadedFile struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

type Runtime struct {
	App                App    `yaml:"app"`
	Daemon             Daemon `yaml:"daemon"`
	SessionsDir        string `yaml:"sessionsDir"`
	CheckoutDir        string `yaml:"checkoutDir"`
	GuardiansURL       string `yaml:"guardiansURL"`
	OMP                OMP    `yaml:"omp"`
	MaxContinuePrompts int    `yaml:"maxContinuePrompts"`
}

type App struct {
	Listen            string `yaml:"listen"`
	WebhookSecretFile string `yaml:"webhookSecretFile"`
	DaemonURL         string `yaml:"daemonURL"`
}

type Daemon struct {
	Listen                 string `yaml:"listen"`
	RebuildIntervalSeconds int    `yaml:"rebuildIntervalSeconds"`
}

// RebuildInterval is the period of the daemon's full queue rebuild.
func (d Daemon) RebuildInterval() time.Duration {
	return time.Duration(d.RebuildIntervalSeconds) * time.Second
}

type OMP struct {
	Executable          string `yaml:"executable"`
	AgentDir            string `yaml:"agentDir"`
	Extension           string `yaml:"extension"`
	ReadyTimeoutSeconds int    `yaml:"readyTimeoutSeconds"`
	ExitGraceSeconds    int    `yaml:"exitGraceSeconds"`
}

// ReadyTimeout bounds the wait for an omp child's ready frame.
func (o OMP) ReadyTimeout() time.Duration { return time.Duration(o.ReadyTimeoutSeconds) * time.Second }

// ExitGrace is how long an omp child may take to exit after its stdin closes.
func (o OMP) ExitGrace() time.Duration { return time.Duration(o.ExitGraceSeconds) * time.Second }

type Council struct {
	Agenda              Agenda     `yaml:"agenda"`
	MaxConcurrentPanels int        `yaml:"maxConcurrentPanels"`
	Resume              Resume     `yaml:"resume"`
	ImplIssue           ImplIssue  `yaml:"implIssue"`
	LaunchLink          LaunchLink `yaml:"launchLink"`
	Reply               Reply      `yaml:"reply"`
	// Archive is nil when decisions are not archived in the PM repository.
	Archive *Archive `yaml:"archive"`
}

// ImplIssue holds the title and body templates of an implementation issue (contract §7).
type ImplIssue struct {
	Title string `yaml:"title"`
	Body  string `yaml:"body"`

	title, body *template.Template
}

// ImplIssueData is what the implementation issue templates see.
type ImplIssueData struct {
	Ref, Agenda string // the agenda issue and its Agenda number
	Row         markers.Row
	// Link is the issue's launch link; empty on the pass that creates the issue.
	Link string
	// Replaces is the implementation issue and ask this issue replaces, or nil.
	Replaces *ReplacesData
}

type ReplacesData struct {
	Old string
	Ask int64
}

// RenderTitle executes the title template.
func (t *ImplIssue) RenderTitle(d ImplIssueData) (string, error) { return render(t.title, d) }

// RenderBody executes the body template.
func (t *ImplIssue) RenderBody(d ImplIssueData) (string, error) { return render(t.body, d) }

// LaunchLink is the omp-dispatch launch link of an implementation issue (contract §7):
// <base>/<owner>/<repo>/issues/<n>?spec=<{"cwd":<cwd>,"frontend":<frontend>}>.
type LaunchLink struct {
	Base     string   `yaml:"base"`
	Frontend Frontend `yaml:"frontend"`
}

type Frontend struct {
	Kind string `yaml:"kind" json:"kind"`
}

// Link returns the launch link of issue n of repo, started in cwd.
func (l LaunchLink) Link(repo string, n int, cwd string) (string, error) {
	spec, err := json.Marshal(struct {
		Cwd      string   `json:"cwd"`
		Frontend Frontend `json:"frontend"`
	}{cwd, l.Frontend})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/issues/%d?%s", strings.TrimRight(l.Base, "/"), repo, n, url.Values{"spec": {string(spec)}}.Encode()), nil
}

// Reply holds the body template of a pm:reply that settles an escalated ask.
type Reply struct {
	Body string `yaml:"body"`

	body *template.Template
}

// ReplyData is what the reply template sees: the agenda issue, the draft's settle text and the
// decision table.
type ReplyData struct {
	Ref, Agenda, Text, Table string
}

// RenderBody executes the body template.
func (r *Reply) RenderBody(d ReplyData) (string, error) { return render(r.body, d) }

// Archive names the PM-repository directories of decision and vote files.
type Archive struct {
	Decisions string `yaml:"decisions"`
	Votes     string `yaml:"votes"`
}

// Tally is council/tally.yaml.
type Tally struct {
	Threshold   Threshold   `yaml:"threshold"`
	Veto        Veto        `yaml:"veto"`
	Arbitration Arbitration `yaml:"arbitration"`
	MaxRounds   int         `yaml:"maxRounds"`
	// Categories are the row categories a draft may use and a ruling may assign.
	Categories []string `yaml:"categories"`
}

// Threshold is the share of eligible seats whose approval passes a row.
type Threshold struct {
	Numerator   int `yaml:"numerator"`
	Denominator int `yaml:"denominator"`
}

// Of returns ceil(numerator × eligible / denominator).
func (t Threshold) Of(eligible int) int {
	return (t.Numerator*eligible + t.Denominator - 1) / t.Denominator
}

// Veto names the seats whose vetoes can stand and the categories they stand on.
type Veto struct {
	Seats      []string `yaml:"seats"`
	Categories []string `yaml:"categories"`
}

type Arbitration struct {
	Seat string `yaml:"seat"`
}

// Resume is how a panel continues after an interrupt: fork the killed sessions or start fresh ones.
type Resume string

const (
	ResumeFork  Resume = "fork"
	ResumeFresh Resume = "fresh"
)

// Agenda numbers are <Prefix><n> with n >= First.
type Agenda struct {
	Prefix string `yaml:"prefix"`
	First  int    `yaml:"first"`
}

// Right is what a seat may do in a panel.
type Right string

const (
	RightDraft     Right = "draft"
	RightVote      Right = "vote"
	RightVeto      Right = "veto"
	RightArbitrate Right = "arbitrate"
)

var rights = []Right{RightDraft, RightVote, RightVeto, RightArbitrate}

type Seat struct {
	ID     string   `yaml:"id"`
	Prompt string   `yaml:"prompt"`
	Tools  []string `yaml:"tools"`
	Model  string   `yaml:"model"`
	Rights []Right  `yaml:"rights"`
}

// Has reports whether the seat holds right r.
func (s Seat) Has(r Right) bool { return slices.Contains(s.Rights, r) }

// SessionKind is the kind of a seat session.
type SessionKind string

const (
	KindDraft     SessionKind = "draft"
	KindVote      SessionKind = "vote"
	KindRevise    SessionKind = "revise"
	KindArbitrate SessionKind = "arbitrate"
)

// SessionKinds are every session kind, in goals.yaml order.
var SessionKinds = []SessionKind{KindDraft, KindVote, KindRevise, KindArbitrate}

// Council plugin tools (plugin/index.ts).
const (
	ToolReadIssue = "pm_read_issue"
	ToolProxyRead = "pm_proxy_read"
	ToolDraft     = "pm_draft"
	ToolVote      = "pm_vote"
	ToolRuling    = "pm_ruling"
)

// PluginTools are the plugin tools registered in a session of kind k.
func (k SessionKind) PluginTools() []string {
	return append([]string{ToolReadIssue, ToolProxyRead}, k.goalCapable()...)
}

// goalCapable are the plugin tools of kind k whose arguments the program acts on.
func (k SessionKind) goalCapable() []string {
	switch k {
	case KindDraft, KindRevise:
		return []string{ToolDraft}
	case KindVote:
		return []string{ToolVote}
	case KindArbitrate:
		return []string{ToolRuling}
	}
	panic(fmt.Sprintf("unknown session kind %q", k))
}

// Goals holds each session kind's goal tools and prompts (council/goals.yaml).
type Goals struct {
	Draft     Goal `yaml:"draft"`
	Vote      Goal `yaml:"vote"`
	Revise    Goal `yaml:"revise"`
	Arbitrate Goal `yaml:"arbitrate"`
}

// Of returns the goal of kind k.
func (g *Goals) Of(k SessionKind) *Goal {
	switch k {
	case KindDraft:
		return &g.Draft
	case KindVote:
		return &g.Vote
	case KindRevise:
		return &g.Revise
	case KindArbitrate:
		return &g.Arbitrate
	}
	panic(fmt.Sprintf("unknown session kind %q", k))
}

// Goal is one session kind: any of GoalTools succeeding completes the session; Opening is the first
// prompt of a new session, Resume the first prompt of a session forked from a killed one, Continue
// the prompt sent while no goal tool has succeeded.
type Goal struct {
	GoalTools []string `yaml:"goalTools"`
	Opening   string   `yaml:"opening"`
	Continue  string   `yaml:"continue"`
	Resume    string   `yaml:"resume"`

	opening, resume *template.Template
}

// OpeningData is what the opening and resume templates see.
type OpeningData struct {
	Ref, Title, Body, Agenda string
	Round, Version           int
	// MaxRounds is tally.yaml maxRounds.
	MaxRounds int
	// Draft is the draft marker JSON: the draft voted on (vote), revised (revise) or disputed
	// (arbitrate); empty for draft sessions.
	Draft      string
	Escalation *EscalationData
	// Counterexamples are the opposing votes' counterexamples on the draft (revise sessions).
	Counterexamples []CounterexampleData
	// Disputes are the rows to arbitrate with the vetoes that disputed them (arbitrate sessions).
	Disputes []DisputeData
	// Categories are tally.yaml categories.
	Categories []string
}

// EscalationData is the ask an agenda issue was raised from: its implementation issue, the ask
// comment id and the ask comment's text.
type EscalationData struct {
	Issue   string
	Ask     int64
	AskText string
}

// CounterexampleData is one seat's counterexample against one row.
type CounterexampleData struct {
	Seat, Row, Counterexample string
}

// DisputeData is one disputed row: its draft row and the veto that disputes its category.
type DisputeData struct {
	Row                            markers.Row
	Seat, Citation, Classification string
}

// RenderOpening executes the opening template.
func (g *Goal) RenderOpening(d OpeningData) (string, error) { return render(g.opening, d) }

// RenderResume executes the resume template.
func (g *Goal) RenderResume(d OpeningData) (string, error) { return render(g.resume, d) }

func render(t *template.Template, d any) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Content is a category of repositories a seat may have read by proxy.
type Content string

const (
	ContentCode   Content = "code"
	ContentDesign Content = "design"
	ContentOMP    Content = "omp"
)

var contents = []Content{ContentCode, ContentDesign, ContentOMP}

// ProxyReader is a seat's proxy-read mapping (council/proxy-readers.yaml).
type ProxyReader struct {
	Seat    string    `yaml:"seat" json:"seat"`
	Default string    `yaml:"default" json:"default"`
	Others  []string  `yaml:"others" json:"others"`
	Content []Content `yaml:"content" json:"content"`
	Note    string    `yaml:"note" json:"note"`
}

// ProxyReader returns the proxy-read mapping of seat id.
func (c *Config) ProxyReader(seat string) (ProxyReader, bool) {
	i := slices.IndexFunc(c.ProxyReaders, func(p ProxyReader) bool { return p.Seat == seat })
	if i < 0 {
		return ProxyReader{}, false
	}
	return c.ProxyReaders[i], true
}

// DraftSeat returns the one seat with the draft right.
func (c *Config) DraftSeat() Seat {
	return c.Seats[slices.IndexFunc(c.Seats, func(s Seat) bool { return s.Has(RightDraft) })]
}

// Seat returns the seat with id.
func (c *Config) Seat(id string) (Seat, bool) {
	i := slices.IndexFunc(c.Seats, func(s Seat) bool { return s.ID == id })
	if i < 0 {
		return Seat{}, false
	}
	return c.Seats[i], true
}

type Repos struct {
	PM     PMRepo     `yaml:"pm"`
	Code   []CodeRepo `yaml:"code"`
	Design []string   `yaml:"design"`
	OMP    []string   `yaml:"omp"`
}

type PMRepo struct {
	Repo    string     `yaml:"repo"`
	Project ProjectRef `yaml:"project"`
}

type ProjectRef struct {
	Owner  string `yaml:"owner"`
	Number int    `yaml:"number"`
}

type CodeRepo struct {
	Repo string `yaml:"repo"`
	Cwd  string `yaml:"cwd"`
}

// Project maps each semantic Project field to its GitHub field name and, for single-select
// fields, each semantic option to its GitHub option name. Node ids are resolved at startup.
type Project struct {
	Fields ProjectFields `yaml:"fields"`
}

type ProjectFields struct {
	Status   SelectField[StatusOptions]   `yaml:"status"`
	Kind     SelectField[KindOptions]     `yaml:"kind"`
	Agenda   TextField                    `yaml:"agenda"`
	Proposer SelectField[ProposerOptions] `yaml:"proposer"`
}

type SelectField[O any] struct {
	Name    string `yaml:"name"`
	Options O      `yaml:"options"`
}

type TextField struct {
	Name string `yaml:"name"`
}

type StatusOptions struct {
	Todo       string `yaml:"todo"`
	InProgress string `yaml:"inProgress"`
	Done       string `yaml:"done"`
	Superseded string `yaml:"superseded"`
}

type KindOptions struct {
	Human          string `yaml:"human"`
	Agenda         string `yaml:"agenda"`
	Implementation string `yaml:"implementation"`
}

type ProposerOptions struct {
	Human       string `yaml:"human"`
	Guardians   string `yaml:"guardians"`
	CouncilSeat string `yaml:"coordination"` // proposed by a council seat
}

// Named is one semantic option key with its GitHub option name.
type Named struct{ Key, Name string }

// List returns the status options in a fixed order.
func (o StatusOptions) List() []Named {
	return []Named{{"todo", o.Todo}, {"inProgress", o.InProgress}, {"done", o.Done}, {"superseded", o.Superseded}}
}

// List returns the kind options in a fixed order.
func (o KindOptions) List() []Named {
	return []Named{{"human", o.Human}, {"agenda", o.Agenda}, {"implementation", o.Implementation}}
}

// List returns the proposer options in a fixed order.
func (o ProposerOptions) List() []Named {
	return []Named{{"human", o.Human}, {"guardians", o.Guardians}, {"coordination", o.CouncilSeat}}
}

type seatsFile struct {
	Seats []Seat `yaml:"seats"`
}

type proxyReadersFile struct {
	Seats []ProxyReader `yaml:"seats"`
}

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z]+$`)
)

// Load reads and validates the configuration under root.
func Load(root string) (*Config, error) { return load(root, os.DirFS(root)) }

// Check validates candidate configuration files, given by path relative to the config root and by
// content, with the loader Load uses. The candidates are the only files there are: nothing is read
// from disk, and a file not given does not exist. It returns the issues Load would fail with.
func Check(files map[string]string) ([]Issue, error) {
	_, err := load("", memFS(files))
	if err == nil {
		return nil, nil
	}
	leaves := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		leaves = joined.Unwrap()
	}
	issues := make([]Issue, 0, len(leaves))
	for _, e := range leaves {
		var issue Issue
		if !errors.As(e, &issue) {
			return nil, fmt.Errorf("config check: %w", e)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// load reads and validates the configuration files of fsys. root is the config root; it only makes
// the runtime and prompt paths the config stores absolute.
func load(root string, fsys fs.FS) (*Config, error) {
	src := &source{fsys: fsys, blobs: map[string]string{}}
	var (
		rt  Runtime
		cn  Council
		ty  Tally
		sf  seatsFile
		gl  Goals
		pr  proxyReadersFile
		rep Repos
		prj Project
	)
	for _, f := range []struct {
		rel string
		dst any
	}{
		{"council/runtime.yaml", &rt},
		{"council/council.yaml", &cn},
		{"council/tally.yaml", &ty},
		{"council/seats.yaml", &sf},
		{"council/goals.yaml", &gl},
		{"council/proxy-readers.yaml", &pr},
		{"shared/repos.yaml", &rep},
		{"shared/project.yaml", &prj},
	} {
		if err := decodeFile(src, f.rel, f.dst); err != nil {
			return nil, err
		}
	}

	v := &validator{src: src}
	cfg := &Config{Runtime: rt, Council: cn, Tally: ty, Seats: sf.Seats, Goals: gl, ProxyReaders: pr.Seats, Repos: rep, Project: prj}
	councilDir := filepath.Join(root, "council")
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(councilDir, p)
	}

	// runtime.yaml
	{
		f := v.file("council/runtime.yaml")
		r := &cfg.Runtime
		f.str("app.listen", r.App.Listen)
		f.str("app.webhookSecretFile", r.App.WebhookSecretFile)
		f.url("app.daemonURL", r.App.DaemonURL)
		f.str("daemon.listen", r.Daemon.Listen)
		f.positive("daemon.rebuildIntervalSeconds", r.Daemon.RebuildIntervalSeconds)
		f.str("sessionsDir", r.SessionsDir)
		f.str("checkoutDir", r.CheckoutDir)
		f.url("guardiansURL", r.GuardiansURL)
		f.str("omp.executable", r.OMP.Executable)
		f.str("omp.agentDir", r.OMP.AgentDir)
		f.str("omp.extension", r.OMP.Extension)
		f.positive("omp.readyTimeoutSeconds", r.OMP.ReadyTimeoutSeconds)
		f.positive("omp.exitGraceSeconds", r.OMP.ExitGraceSeconds)
		f.positive("maxContinuePrompts", r.MaxContinuePrompts)
		r.App.WebhookSecretFile = resolve(r.App.WebhookSecretFile)
		r.SessionsDir = resolve(r.SessionsDir)
		r.CheckoutDir = resolve(r.CheckoutDir)
		r.OMP.AgentDir = resolve(r.OMP.AgentDir)
		r.OMP.Extension = resolve(r.OMP.Extension)
	}

	// council.yaml
	{
		f := v.file("council/council.yaml")
		a := cfg.Council.Agenda
		if f.str("agenda.prefix", a.Prefix) && !prefixPattern.MatchString(a.Prefix) {
			f.errorf("agenda.prefix", "%q must be letters only", a.Prefix)
		}
		f.positive("agenda.first", a.First)
		f.positive("maxConcurrentPanels", cfg.Council.MaxConcurrentPanels)
		switch cfg.Council.Resume {
		case ResumeFork, ResumeFresh:
		case "":
			f.missing("resume")
		default:
			f.errorf("resume", "%q is not one of fork, fresh", cfg.Council.Resume)
		}
		c := &cfg.Council
		c.ImplIssue.title = f.template("implIssue.title", c.ImplIssue.Title, implIssueSamples()...)
		c.ImplIssue.body = f.template("implIssue.body", c.ImplIssue.Body, implIssueSamples()...)
		f.url("launchLink.base", c.LaunchLink.Base)
		f.str("launchLink.frontend.kind", c.LaunchLink.Frontend.Kind)
		c.Reply.body = f.template("reply.body", c.Reply.Body, ReplyData{Ref: "o/r#1", Agenda: "A1", Text: "t", Table: "|"})
		if a := c.Archive; a != nil {
			f.repoPath("archive.decisions", a.Decisions)
			f.repoPath("archive.votes", a.Votes)
		}
	}

	// seats.yaml
	{
		f := v.file("council/seats.yaml")
		f.nonEmpty("seats", len(cfg.Seats))
		ids := map[string]bool{}
		for i, s := range cfg.Seats {
			p := fmt.Sprintf("seats[%d]", i)
			switch {
			case s.ID == "":
				f.missing(p + ".id")
			case !idPattern.MatchString(s.ID):
				f.errorf(p+".id", "%q does not match [A-Za-z0-9_-]+", s.ID)
			case ids[s.ID]:
				f.errorf(p+".id", "duplicate seat %q", s.ID)
			}
			ids[s.ID] = true
			if f.str(p+".prompt", s.Prompt) {
				if filepath.IsAbs(s.Prompt) {
					f.errorf(p+".prompt", "%q must be relative to council/", s.Prompt)
				} else {
					cfg.Seats[i].Prompt = resolve(s.Prompt)
					f.exists(p+".prompt", path.Join("council", s.Prompt))
				}
			}
			f.nonEmpty(p+".tools", len(s.Tools))
			for j, t := range s.Tools {
				f.str(fmt.Sprintf("%s.tools[%d]", p, j), t)
			}
			f.str(p+".model", s.Model)
			f.nonEmpty(p+".rights", len(s.Rights))
			seen := map[Right]bool{}
			for j, r := range s.Rights {
				rp := fmt.Sprintf("%s.rights[%d]", p, j)
				switch {
				case !slices.Contains(rights, r):
					f.errorf(rp, "%q is not one of draft, vote, veto, arbitrate", r)
				case seen[r]:
					f.errorf(rp, "duplicate right %q", r)
				}
				seen[r] = true
			}
		}
		drafters := 0
		voters := 0
		for _, s := range cfg.Seats {
			if s.Has(RightDraft) {
				drafters++
			}
			if s.Has(RightVote) {
				voters++
			}
		}
		if len(cfg.Seats) > 0 && drafters != 1 {
			f.errorf("seats", "exactly one seat must have the draft right, %d have", drafters)
		}
		if len(cfg.Seats) > 0 && voters == 0 {
			f.errorf("seats", "no seat has the vote right")
		}
	}

	// tally.yaml
	{
		f := v.file("council/tally.yaml")
		t := cfg.Tally
		f.positive("threshold.numerator", t.Threshold.Numerator)
		f.positive("threshold.denominator", t.Threshold.Denominator)
		if t.Threshold.Numerator > t.Threshold.Denominator {
			f.errorf("threshold", "numerator %d exceeds denominator %d", t.Threshold.Numerator, t.Threshold.Denominator)
		}
		f.positive("maxRounds", t.MaxRounds)
		f.nonEmpty("categories", len(t.Categories))
		cats := map[string]bool{}
		for i, c := range t.Categories {
			cp := fmt.Sprintf("categories[%d]", i)
			if !f.str(cp, c) {
				continue
			}
			if cats[c] {
				f.errorf(cp, "duplicate category %q", c)
			}
			cats[c] = true
		}
		seat := func(path, id string, r Right) {
			if !f.str(path, id) {
				return
			}
			switch s, ok := cfg.Seat(id); {
			case !ok:
				f.errorf(path, "%q is not a seat of council/seats.yaml", id)
			case !s.Has(r):
				f.errorf(path, "seat %q does not have the %s right", id, r)
			}
		}
		f.present("veto.seats", t.Veto.Seats != nil)
		for i, id := range t.Veto.Seats {
			seat(fmt.Sprintf("veto.seats[%d]", i), id, RightVeto)
		}
		f.present("veto.categories", t.Veto.Categories != nil)
		for i, c := range t.Veto.Categories {
			if cp := fmt.Sprintf("veto.categories[%d]", i); f.str(cp, c) && !cats[c] {
				f.errorf(cp, "%q is not one of categories", c)
			}
		}
		seat("arbitration.seat", t.Arbitration.Seat, RightArbitrate)
	}

	// goals.yaml
	{
		f := v.file("council/goals.yaml")
		for _, k := range SessionKinds {
			g := cfg.Goals.Of(k)
			p := string(k)
			f.nonEmpty(p+".goalTools", len(g.GoalTools))
			seen := map[string]bool{}
			for j, t := range g.GoalTools {
				tp := fmt.Sprintf("%s.goalTools[%d]", p, j)
				switch {
				case !slices.Contains(k.goalCapable(), t):
					f.errorf(tp, "%q is not a %s goal tool of the council plugin (%s)", t, k, strings.Join(k.goalCapable(), ", "))
				case seen[t]:
					f.errorf(tp, "duplicate goal tool %q", t)
				}
				seen[t] = true
			}
			f.str(p+".continue", g.Continue)
			g.opening = f.template(p+".opening", g.Opening, openingSamples()...)
			g.resume = f.template(p+".resume", g.Resume, openingSamples()...)
		}
	}

	// proxy-readers.yaml
	{
		f := v.file("council/proxy-readers.yaml")
		f.present("seats", cfg.ProxyReaders != nil)
		seen := map[string]bool{}
		for i, r := range cfg.ProxyReaders {
			p := fmt.Sprintf("seats[%d]", i)
			if f.str(p+".seat", r.Seat) {
				switch _, ok := cfg.Seat(r.Seat); {
				case !ok:
					f.errorf(p+".seat", "%q is not a seat of council/seats.yaml", r.Seat)
				case seen[r.Seat]:
					f.errorf(p+".seat", "duplicate seat %q", r.Seat)
				}
				seen[r.Seat] = true
			}
			f.str(p+".default", r.Default)
			f.present(p+".others", r.Others != nil)
			for j, m := range r.Others {
				f.str(fmt.Sprintf("%s.others[%d]", p, j), m)
			}
			f.nonEmpty(p+".content", len(r.Content))
			kinds := map[Content]bool{}
			for j, c := range r.Content {
				cp := fmt.Sprintf("%s.content[%d]", p, j)
				switch {
				case !slices.Contains(contents, c):
					f.errorf(cp, "%q is not one of code, design, omp", c)
				case kinds[c]:
					f.errorf(cp, "duplicate content %q", c)
				}
				kinds[c] = true
			}
			f.str(p+".note", r.Note)
		}
		for _, s := range cfg.Seats {
			if (s.Has(RightDraft) || s.Has(RightVote)) && s.ID != "" && !seen[s.ID] {
				f.errorf("seats", "seat %q drafts or votes and has no entry", s.ID)
			}
		}
	}

	// shared/repos.yaml
	{
		f := v.file("shared/repos.yaml")
		f.repo("pm.repo", rep.PM.Repo)
		f.str("pm.project.owner", rep.PM.Project.Owner)
		f.positive("pm.project.number", rep.PM.Project.Number)
		f.present("code", rep.Code != nil)
		for i, c := range rep.Code {
			f.repo(fmt.Sprintf("code[%d].repo", i), c.Repo)
			f.str(fmt.Sprintf("code[%d].cwd", i), c.Cwd)
		}
		f.present("design", rep.Design != nil)
		for i, name := range rep.Design {
			f.repo(fmt.Sprintf("design[%d]", i), name)
		}
		f.present("omp", rep.OMP != nil)
		for i, name := range rep.OMP {
			f.repo(fmt.Sprintf("omp[%d]", i), name)
		}
	}

	// shared/project.yaml
	{
		f := v.file("shared/project.yaml")
		p := cfg.Project.Fields
		f.selectField("fields.status", p.Status.Name, p.Status.Options.List())
		f.selectField("fields.kind", p.Kind.Name, p.Kind.Options.List())
		f.str("fields.agenda.name", p.Agenda.Name)
		f.selectField("fields.proposer", p.Proposer.Name, p.Proposer.Options.List())
	}

	if err := errors.Join(v.errs...); err != nil {
		return nil, err
	}
	cfg.Loaded = Loaded{LoadedAt: time.Now().UTC(), Files: src.files()}
	return cfg, nil
}

// validator collects the load errors of one load; files are read through src.
type validator struct {
	errs []error
	src  *source
}

// source reads configuration files from fsys and records the git blob sha of each file it reads.
type source struct {
	fsys  fs.FS
	blobs map[string]string
}

// read returns the content of rel, a path relative to the config root, and records its blob sha.
func (s *source) read(rel string) ([]byte, error) {
	b, err := fs.ReadFile(s.fsys, rel)
	if err != nil {
		return nil, err
	}
	s.blobs[rel] = blobSHA(b)
	return b, nil
}

// files lists the files read so far, sorted by path.
func (s *source) files() []LoadedFile {
	out := make([]LoadedFile, 0, len(s.blobs))
	for p, sha := range s.blobs {
		out = append(out, LoadedFile{Path: p, SHA: sha})
	}
	slices.SortFunc(out, func(a, b LoadedFile) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// blobSHA is the git blob sha of content b: sha1 over "blob <len>\x00" followed by b.
func blobSHA(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// memFS is a read-only in-memory fs.FS over the files of a check; a directory exists where some
// file lies beneath it.
type memFS map[string]string

func (m memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if s, ok := m[name]; ok {
		return &memFile{info: memInfo{name: path.Base(name), size: int64(len(s))}, r: strings.NewReader(s)}, nil
	}
	for k := range m {
		if strings.HasPrefix(k, name+"/") {
			return &memFile{info: memInfo{name: path.Base(name), dir: true}}, nil
		}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// memFile is an open memFS file (r set) or directory.
type memFile struct {
	info memInfo
	r    *strings.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *memFile) Read(b []byte) (int, error) {
	if f.r == nil {
		return 0, &fs.PathError{Op: "read", Path: f.info.name, Err: errIsDir}
	}
	return f.r.Read(b)
}

func (f *memFile) Close() error { return nil }

var errIsDir = errors.New("is a directory")

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string { return i.name }

func (i memInfo) Size() int64 { return i.size }

func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

func (i memInfo) ModTime() time.Time { return time.Time{} }

func (i memInfo) IsDir() bool { return i.dir }

func (i memInfo) Sys() any { return nil }

type fileValidator struct {
	v    *validator
	name string
}

func (v *validator) file(name string) fileValidator { return fileValidator{v, name} }

func (f fileValidator) errorf(path, format string, args ...any) {
	f.v.errs = append(f.v.errs, Issue{File: f.name, Key: path, Message: fmt.Sprintf(format, args...)})
}

func (f fileValidator) missing(path string) { f.errorf(path, "missing") }

func (f fileValidator) str(path, s string) bool {
	if strings.TrimSpace(s) == "" {
		f.missing(path)
		return false
	}
	return true
}

func (f fileValidator) present(path string, ok bool) {
	if !ok {
		f.missing(path)
	}
}

func (f fileValidator) nonEmpty(path string, n int) {
	if n == 0 {
		f.missing(path)
	}
}

func (f fileValidator) positive(path string, n int) {
	if n <= 0 {
		f.errorf(path, "missing (must be a positive integer)")
	}
}

func (f fileValidator) repo(path, s string) bool {
	if !f.str(path, s) {
		return false
	}
	if !repoPattern.MatchString(s) {
		f.errorf(path, "%q is not owner/name", s)
		return false
	}
	return true
}

func (f fileValidator) url(path, s string) {
	if f.str(path, s) && !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		f.errorf(path, "%q is not an http(s) URL", s)
	}
}

// exists reports unless rel, a file relative to the config root, is a readable file; it records the file.
func (f fileValidator) exists(path, rel string) {
	st, err := fs.Stat(f.v.src.fsys, rel)
	if err != nil {
		f.errorf(path, "%v", err)
	} else if st.IsDir() {
		f.errorf(path, "%s is a directory", rel)
	} else if _, err := f.v.src.read(rel); err != nil {
		f.errorf(path, "%v", err)
	}
}

// repoPath checks a directory of the PM repository: relative, slash-separated and clean.
func (f fileValidator) repoPath(key, s string) {
	if f.str(key, s) && (path.IsAbs(s) || path.Clean(s) != s || s == "." || s == ".." || strings.HasPrefix(s, "../")) {
		f.errorf(key, "%q is not a clean relative directory of the repository", s)
	}
}

// template parses s as a text/template and executes it with each sample, so that a field the
// data does not have, or a nil the template does not guard, is a load error, not a panel failure.
func (f fileValidator) template(path, s string, samples ...any) *template.Template {
	if !f.str(path, s) {
		return nil
	}
	t, err := template.New(path).Option("missingkey=error").Parse(s)
	if err != nil {
		f.errorf(path, "%v", err)
		return nil
	}
	for _, sample := range samples {
		if _, err := render(t, sample); err != nil {
			f.errorf(path, "%v", err)
			return nil
		}
	}
	return t
}

// openingSamples are opening data with and without an escalation, counterexamples and disputes.
func openingSamples() []any {
	row := sampleRow()
	full := OpeningData{
		Ref: "o/r#1", Title: "t", Body: "b", Agenda: "A1", Round: 2, Version: 2, MaxRounds: 2, Draft: "{}",
		Escalation:      &EscalationData{Issue: "o/c#1", Ask: 1, AskText: "a"},
		Counterexamples: []CounterexampleData{{Seat: "S", Row: row.ID, Counterexample: "c"}},
		Disputes:        []DisputeData{{Row: row, Seat: "S", Citation: "o/r#2", Classification: "k"}},
		Categories:      []string{"k"},
	}
	bare := OpeningData{Ref: "o/r#1", Title: "t", Body: "b", Agenda: "A1", Round: 1, Version: 1, MaxRounds: 2}
	return []any{bare, full}
}

// implIssueSamples are implementation issue data on the create and edit passes, with and
// without a replaced issue.
func implIssueSamples() []any {
	var out []any
	for _, link := range []string{"", "http://h/o/c/issues/1?spec=x"} {
		for _, rep := range []*ReplacesData{nil, {Old: "o/c#2", Ask: 1}} {
			out = append(out, ImplIssueData{Ref: "o/r#1", Agenda: "A1", Row: sampleRow(), Link: link, Replaces: rep})
		}
	}
	return out
}

func sampleRow() markers.Row {
	return markers.Row{ID: "A1-01", What: "w", Where: "p", How: "h", Tradeoff: "t", Category: "k",
		Deliver: &markers.Deliver{Repo: "o/c", Title: "t", Body: "b"}}
}

func (f fileValidator) selectField(path, name string, options []Named) {
	f.str(path+".name", name)
	names := map[string]bool{}
	for _, o := range options {
		op := path + ".options." + o.Key
		if !f.str(op, o.Name) {
			continue
		}
		if names[o.Name] {
			f.errorf(op, "option %q is used twice", o.Name)
		}
		names[o.Name] = true
	}
}

// decodeFile strictly decodes the file rel of src into dst. Decode errors are reported with the YAML key path.
func decodeFile(src *source, rel string, dst any) error {
	b, err := src.read(rel)
	if err != nil {
		return Issue{File: rel, Message: err.Error()}
	}
	err = yaml.UnmarshalWithOptions(b, dst, yaml.DisallowUnknownField(), yaml.Strict())
	if err == nil {
		return nil
	}
	var ye interface {
		GetMessage() string
		GetToken() *token.Token
	}
	if errors.As(err, &ye) && ye.GetToken() != nil {
		if path := keyPath(b, ye.GetToken().Position); path != "" {
			return Issue{File: rel, Key: path, Message: ye.GetMessage()}
		}
		return Issue{File: rel, Message: fmt.Sprintf("line %d: %s", ye.GetToken().Position.Line, ye.GetMessage())}
	}
	return Issue{File: rel, Message: err.Error()}
}

// keyPath returns the YAML path (without the leading "$.") of the node at pos.
func keyPath(src []byte, pos *token.Position) string {
	file, err := parser.ParseBytes(src, 0)
	if err != nil || pos == nil {
		return ""
	}
	var found string
	for _, doc := range file.Docs {
		ast.Walk(visitFunc(func(n ast.Node) bool {
			if found != "" {
				return false
			}
			if tk := n.GetToken(); tk != nil && tk.Position.Line == pos.Line && tk.Position.Column == pos.Column {
				found = n.GetPath()
				return false
			}
			return true
		}), doc)
	}
	found = strings.TrimPrefix(strings.TrimPrefix(found, "$"), ".")
	return found
}

type visitFunc func(ast.Node) bool

func (f visitFunc) Visit(n ast.Node) ast.Visitor {
	if f(n) {
		return f
	}
	return nil
}
