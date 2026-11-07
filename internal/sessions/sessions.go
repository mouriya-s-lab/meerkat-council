// Package sessions runs council seat sessions and derives session records from the omp session
// files under the sessions directory (contract §4).
package sessions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oh-my-omp/pm-guardians/omprpc"

	"github.com/oh-my-omp/pm-council/internal/config"
)

// Meta is the pm-meta custom entry the council plugin appends at session start.
type Meta struct {
	Kind    config.SessionKind `json:"kind"`
	Seat    string             `json:"seat"`
	Model   string             `json:"model"`
	Ref     string             `json:"ref"`
	Round   int                `json:"round"`
	Version int                `json:"version"`
}

// File is what the program derives from one omp session file. Entries timestamped before the
// session header (history copied by a fork) count only for Meta.
type File struct {
	Path          string
	ID            string
	ParentSession string
	StartedAt     time.Time
	LastAt        time.Time
	Meta          *Meta                // latest pm-meta entry
	ToolOK        map[string]time.Time // tool name -> timestamp of its latest successful tool result
	LastError     string               // errorMessage of the latest assistant message that failed
}

// ErrNotSession marks a .jsonl file that is not an omp session file (no session header).
var ErrNotSession = errors.New("not an omp session file")

const maxLineBytes = 64 << 20

type entry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	Timestamp     string          `json:"timestamp"`
	ParentSession string          `json:"parentSession"`
	CustomType    string          `json:"customType"`
	Data          json.RawMessage `json:"data"`
	Message       *struct {
		Role         string `json:"role"`
		ToolName     string `json:"toolName"`
		IsError      bool   `json:"isError"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

// ParseFile reads an omp session file. Lines before the header may only be title lines.
func ParseFile(path string) (*File, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	f := &File{Path: path, ToolOK: map[string]time.Time{}}
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a line being written concurrently
		}
		if f.ID == "" {
			switch e.Type {
			case "title":
				continue
			case "session":
				f.ID = e.ID
				f.ParentSession = e.ParentSession
				f.StartedAt = parseTime(e.Timestamp)
				f.LastAt = f.StartedAt
				continue
			default:
				return nil, ErrNotSession
			}
		}
		ts := parseTime(e.Timestamp)
		if ts.After(f.LastAt) {
			f.LastAt = ts
		}
		if e.Type == "custom" && e.CustomType == "pm-meta" {
			var m Meta
			if json.Unmarshal(e.Data, &m) == nil {
				f.Meta = &m
			}
			continue
		}
		if e.Type != "message" || e.Message == nil || ts.Before(f.StartedAt) {
			continue
		}
		switch e.Message.Role {
		case "toolResult":
			if !e.Message.IsError {
				f.ToolOK[e.Message.ToolName] = ts
			}
		case "assistant":
			if e.Message.ErrorMessage != "" {
				f.LastError = e.Message.ErrorMessage
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if f.ID == "" {
		return nil, ErrNotSession
	}
	return f, nil
}

// ScanDir parses every session file under dir (recursively); other .jsonl files are skipped.
// A missing dir yields no files.
func ScanDir(dir string) ([]*File, error) {
	var files []*File
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		f, err := ParseFile(path)
		if errors.Is(err, ErrNotSession) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	return files, err
}

// Spec is one seat session to run until one of its goal tools succeeds.
type Spec struct {
	OMP                        config.OMP
	Model, SystemPrompt, Tools string
	SessionDir, Cwd            string
	ForkFrom                   string // killed session file to fork; empty for a new session
	GoalTools                  []string
	Opening, Continue          string
	Meta                       Meta
	ContextFile                string // PM_CONTEXT
	GuardiansURL               string // PM_GUARDIANS_URL
	MaxContinuePrompts         int
	// OnReady, if set, is called once the session id and file are known.
	OnReady func(sessionID, sessionFile string)
}

// Result is a session whose goal tool succeeded: ToolName and Args are that call's.
type Result struct {
	SessionID, SessionFile, ToolName string
	Args                             json.RawMessage
}

// Failure is a session that ended without a goal tool succeeding. Reason is omprpc's verbatim.
type Failure struct{ Reason string }

func (f *Failure) Error() string { return f.Reason }

// Registry runs sessions and remembers, for this process lifetime, which are live and why failed
// ones failed. Everything else about a session comes from its file.
type Registry struct {
	mu       sync.Mutex
	live     map[string]bool
	failures map[string]string
}

func NewRegistry() *Registry {
	return &Registry{live: map[string]bool{}, failures: map[string]string{}}
}

// Run starts omp per s through omprpc.RunSession and records, while it runs, that the session is
// live and, if it fails, why.
func (r *Registry) Run(ctx context.Context, s Spec) (Result, error) {
	if err := os.MkdirAll(s.SessionDir, 0o755); err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	meta, err := json.Marshal(s.Meta)
	if err != nil {
		return Result{}, &Failure{Reason: err.Error()}
	}
	args := []string{
		"--mode", "rpc",
		"--session-dir", s.SessionDir,
		"--model", s.Model,
		"--system-prompt", s.SystemPrompt,
		"--tools", s.Tools,
		"--trusted-extension", s.OMP.Extension,
		"--no-skills", "--no-rules", "--no-lsp",
	}
	if s.ForkFrom != "" {
		args = append(args, "--fork", s.ForkFrom)
	}
	var sessionID string
	res, err := omprpc.RunSession(ctx, omprpc.SessionSpec{
		Options: omprpc.Options{
			Bin:  s.OMP.Executable,
			Args: args,
			Dir:  s.Cwd,
			Env: []string{
				"PI_CODING_AGENT_DIR=" + s.OMP.AgentDir,
				"PM_KIND=" + string(s.Meta.Kind),
				"PM_GOAL_TOOLS=" + strings.Join(s.GoalTools, ","),
				"PM_CONTINUE=" + s.Continue,
				"PM_META=" + string(meta),
				"PM_CONTEXT=" + s.ContextFile,
				"PM_GUARDIANS_URL=" + s.GuardiansURL,
			},
		},
		Opening:            s.Opening,
		Continue:           s.Continue,
		GoalTools:          s.GoalTools,
		MaxContinuePrompts: s.MaxContinuePrompts,
		ReadyTimeout:       s.OMP.ReadyTimeout(),
		ExitGrace:          s.OMP.ExitGrace(),
		OnReady: func(id, file string) {
			sessionID = id
			r.mu.Lock()
			r.live[id] = true
			r.mu.Unlock()
			if s.OnReady != nil {
				s.OnReady(id, file)
			}
		},
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID != "" {
		delete(r.live, sessionID)
	}
	if err != nil {
		if sessionID != "" {
			r.failures[sessionID] = err.Error()
		}
		return Result{}, &Failure{Reason: err.Error()}
	}
	return Result{SessionID: res.SessionID, SessionFile: res.SessionFile, ToolName: res.ToolName, Args: res.Args}, nil
}

// State is the §4 session state.
type State string

const (
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Summary is one row of GET /v1/sessions.
type Summary struct {
	ID        string             `json:"id"`
	Kind      config.SessionKind `json:"kind"`
	Label     string             `json:"label"`
	Model     string             `json:"model"`
	StartedAt time.Time          `json:"startedAt"`
	EndedAt   *time.Time         `json:"endedAt"`
	State     State              `json:"state"`
	Error     *string            `json:"error"`
	Ref       *string            `json:"ref"`
	Parent    *string            `json:"parent"`
}

// List derives session rows from every session file under dir, newest first, at most limit rows.
// A session is running while it is live in this process, done when one of its kind's goal tools
// has a successful result, failed otherwise.
func (r *Registry) List(dir string, goals config.Goals, limit int) ([]Summary, error) {
	files, err := ScanDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].StartedAt.After(files[j].StartedAt) })
	if len(files) > limit {
		files = files[:limit]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Summary, 0, len(files))
	for _, f := range files {
		s := Summary{ID: f.ID, StartedAt: f.StartedAt}
		var label []string
		var goalTools []string
		if m := f.Meta; m != nil {
			s.Kind, s.Model = m.Kind, m.Model
			label = append(label, string(m.Kind))
			if m.Seat != "" {
				label = append(label, m.Seat)
			}
			if m.Ref != "" {
				s.Ref = new(m.Ref)
				label = append(label, m.Ref)
			}
			if slices.Contains(config.SessionKinds, m.Kind) {
				goalTools = goals.Of(m.Kind).GoalTools
			}
		}
		s.Label = strings.Join(label, " ")
		if f.ParentSession != "" {
			s.Parent = new(f.ParentSession)
		}
		done := slices.ContainsFunc(goalTools, func(t string) bool { _, ok := f.ToolOK[t]; return ok })
		switch {
		case r.live[f.ID]:
			s.State = StateRunning
		case done:
			s.State = StateDone
		default:
			s.State = StateFailed
			reason, ok := r.failures[f.ID]
			switch {
			case ok:
			case f.LastError != "":
				reason = f.LastError
			default:
				reason = "ended without a goal tool"
			}
			s.Error = &reason
		}
		if s.State != StateRunning {
			s.EndedAt = new(f.LastAt)
		}
		out = append(out, s)
	}
	return out, nil
}

// FindFile returns the session file under dir whose header id is id.
func FindFile(dir, id string) (string, bool, error) {
	files, err := ScanDir(dir)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		if f.ID == id {
			return f.Path, true, nil
		}
	}
	return "", false, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
