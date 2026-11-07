// Package sessions runs council seat sessions and derives session records from the omp session
// files under the sessions directory (contract §4).
package sessions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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

// File is what the program derives from one omp session file and the failure record the program
// keeps beside it. Entries timestamped before the session header (history copied by a fork) count
// only for Meta.
type File struct {
	Path          string
	ID            string
	ParentSession string
	StartedAt     time.Time
	LastAt        time.Time
	Meta          *Meta                // latest pm-meta entry
	ToolOK        map[string]time.Time // tool name -> timestamp of its latest successful tool result
	LastError     string               // errorMessage of the latest assistant message that failed
	Failure       string               // why the program's run of this session failed; see FailurePath
	FailedAt      time.Time            // when the program decided it failed; zero for records without it
}

// failureRecord is the file at FailurePath. Records written before At existed lack it.
type failureRecord struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at,omitzero"` // RFC 3339
}

// FailurePath is where the program records why and when its run of the session in the omp session
// file path failed (omp exited, a non-retryable error, the continue limit, the session timeout). omp
// writes the session file and stops writing it when killed; the program owns this record, so the
// reason and the end time outlive the process.
func FailurePath(sessionFile string) string {
	return strings.TrimSuffix(sessionFile, ".jsonl") + ".pm-failure.json"
}

// ErrShutdown is the cancel cause of a daemon stop. Shutdown carries the time of the cancel.
var ErrShutdown = errors.New("daemon shutdown")

// shutdownReason is the failure a session killed by a daemon stop records.
const shutdownReason = "守护进程关闭导致会话被终止"

// shutdown is the cancel cause of a daemon stop decided at at.
type shutdown struct{ at time.Time }

func (s shutdown) Error() string { return ErrShutdown.Error() }

func (s shutdown) Unwrap() error { return ErrShutdown }

// Shutdown returns the cancel cause of a daemon stop decided at at.
func Shutdown(at time.Time) error { return shutdown{at: at} }

// ShutdownAt reports the cancel time of a daemon stop, when cause is one.
func ShutdownAt(cause error) (time.Time, bool) {
	var stop shutdown
	if errors.As(cause, &stop) {
		return stop.at, true
	}
	return time.Time{}, false
}

// failureOf is the failure a session whose run returned err records, given its ctx's cancel cause: a
// daemon stop names the stop and its cancel time, anything else is err at now.
func failureOf(err, cause error, now time.Time) (reason string, at time.Time) {
	if at, ok := ShutdownAt(cause); ok {
		return shutdownReason, at
	}
	return err.Error(), now
}

// RecordFailure records that the session in sessionFile failed at at for reason, atomically.
func RecordFailure(sessionFile, reason string, at time.Time) error {
	b, err := json.Marshal(failureRecord{Reason: reason, At: at})
	if err != nil {
		return err
	}
	path := FailurePath(sessionFile)
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
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
	switch b, err := os.ReadFile(FailurePath(path)); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var rec failureRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", FailurePath(path), err)
		}
		f.Failure, f.FailedAt = rec.Reason, rec.At
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

// Registry runs sessions and tracks, for this process lifetime, which are live. Why a failed
// session failed is kept beside its session file; everything else about a session comes from it.
type Registry struct {
	mu   sync.Mutex
	live map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{live: map[string]bool{}}
}

// Run starts omp per s through omprpc.RunSession and records, while it runs, that the session is
// live; if it fails after its file is known, the failure goes into the record beside that file.
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
	var sessionID, sessionFile string
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
			sessionID, sessionFile = id, file
			r.mu.Lock()
			r.live[id] = true
			r.mu.Unlock()
			if s.OnReady != nil {
				s.OnReady(id, file)
			}
		},
	})
	if err != nil && sessionFile != "" {
		reason, at := failureOf(err, context.Cause(ctx), time.Now().UTC())
		if werr := RecordFailure(sessionFile, reason, at); werr != nil {
			slog.Error("record session failure", "session", sessionID, "err", werr)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID != "" {
		delete(r.live, sessionID)
	}
	if err != nil {
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
			reason := "ended without a goal tool"
			switch {
			case f.Failure != "":
				reason = f.Failure
			case f.LastError != "":
				reason = f.LastError
			}
			s.Error = &reason
		}
		if s.State != StateRunning {
			s.EndedAt = new(f.LastAt)
			if s.State == StateFailed && !f.FailedAt.IsZero() {
				s.EndedAt = new(f.FailedAt)
			}
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
