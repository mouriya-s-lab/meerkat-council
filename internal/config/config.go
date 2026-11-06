// Package config loads and validates the council configuration under <config-root>:
// council/{runtime,council,seats}.yaml and shared/{repos,project}.yaml.
// Unknown keys and missing required keys are errors that name the file and the YAML key path.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// Config is the validated configuration.
type Config struct {
	Runtime Runtime
	Council Council
	Seats   []Seat
	Repos   Repos
	Project Project
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

type Council struct {
	Agenda              Agenda `yaml:"agenda"`
	MaxConcurrentPanels int    `yaml:"maxConcurrentPanels"`
}

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
	Human        string `yaml:"human"`
	Guardians    string `yaml:"guardians"`
	Coordination string `yaml:"coordination"`
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
	return []Named{{"human", o.Human}, {"guardians", o.Guardians}, {"coordination", o.Coordination}}
}

type seatsFile struct {
	Seats []Seat `yaml:"seats"`
}

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z]+$`)
)

// Load reads and validates the configuration under root.
func Load(root string) (*Config, error) {
	var (
		rt  Runtime
		cn  Council
		sf  seatsFile
		rep Repos
		prj Project
	)
	for _, f := range []struct {
		rel string
		dst any
	}{
		{"council/runtime.yaml", &rt},
		{"council/council.yaml", &cn},
		{"council/seats.yaml", &sf},
		{"shared/repos.yaml", &rep},
		{"shared/project.yaml", &prj},
	} {
		if err := decodeFile(root, f.rel, f.dst); err != nil {
			return nil, err
		}
	}

	v := &validator{}
	cfg := &Config{Runtime: rt, Council: cn, Seats: sf.Seats, Repos: rep, Project: prj}
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
			if f.str(p+".prompt", s.Prompt) && filepath.IsAbs(s.Prompt) {
				f.errorf(p+".prompt", "%q must be relative to council/", s.Prompt)
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
	return cfg, nil
}

type validator struct{ errs []error }

type fileValidator struct {
	v    *validator
	name string
}

func (v *validator) file(name string) fileValidator { return fileValidator{v, name} }

func (f fileValidator) errorf(path, format string, args ...any) {
	f.v.errs = append(f.v.errs, fmt.Errorf("%s: %s: %s", f.name, path, fmt.Sprintf(format, args...)))
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

// decodeFile strictly decodes root/rel into dst. Decode errors are reported with the YAML key path.
func decodeFile(root, rel string, dst any) error {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
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
			return fmt.Errorf("%s: %s: %s", rel, path, ye.GetMessage())
		}
		return fmt.Errorf("%s: line %d: %s", rel, ye.GetToken().Position.Line, ye.GetMessage())
	}
	return fmt.Errorf("%s: %w", rel, err)
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
