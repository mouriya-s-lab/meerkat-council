// Package sessions derives council session records from the omp session files under the
// sessions directory (contract §4).
package sessions

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Meta is the pm-meta custom entry the council plugin appends at session start.
type Meta struct {
	Kind  string `json:"kind"`
	Seat  string `json:"seat,omitempty"`
	Model string `json:"model"`
	Ref   string `json:"ref,omitempty"`
}

// File is what the program derives from one omp session file. Entries timestamped before the
// session header (history copied by a fork) count only for Meta.
type File struct {
	Path      string
	ID        string
	StartedAt time.Time
	LastAt    time.Time
	Meta      *Meta  // latest pm-meta entry
	Exited    bool   // a session_exit entry was written
	LastError string // errorMessage of the latest assistant message that failed
}

// ErrNotSession marks a .jsonl file that is not an omp session file (no session header).
var ErrNotSession = errors.New("not an omp session file")

const maxLineBytes = 64 << 20

type entry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Timestamp  string          `json:"timestamp"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
	Message    *struct {
		Role         string `json:"role"`
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
	f := &File{Path: path}
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
		if ts.Before(f.StartedAt) {
			continue
		}
		switch {
		case e.Type == "custom" && e.CustomType == "session_exit":
			f.Exited = true
		case e.Type == "message" && e.Message != nil && e.Message.Role == "assistant" && e.Message.ErrorMessage != "":
			f.LastError = e.Message.ErrorMessage
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

// State is the §4 session state.
type State string

const (
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Summary is one row of GET /v1/sessions.
type Summary struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Label     string     `json:"label"`
	Model     string     `json:"model"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	State     State      `json:"state"`
	Error     *string    `json:"error"`
	Ref       *string    `json:"ref"`
}

// List derives session rows from every session file under dir, newest first, at most limit rows.
// A session is running until omp writes its session_exit entry; it then is failed when its last
// assistant message failed, done otherwise.
func List(dir string, limit int) ([]Summary, error) {
	files, err := ScanDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].StartedAt.After(files[j].StartedAt) })
	if len(files) > limit {
		files = files[:limit]
	}
	out := make([]Summary, 0, len(files))
	for _, f := range files {
		s := Summary{ID: f.ID, StartedAt: f.StartedAt}
		label := []string{}
		if m := f.Meta; m != nil {
			s.Kind, s.Model = m.Kind, m.Model
			label = append(label, m.Kind)
			if m.Seat != "" {
				label = append(label, m.Seat)
			}
			if m.Ref != "" {
				s.Ref = new(m.Ref)
				label = append(label, m.Ref)
			}
		}
		s.Label = strings.Join(label, " ")
		switch {
		case !f.Exited:
			s.State = StateRunning
		case f.LastError != "":
			s.State = StateFailed
		default:
			s.State = StateDone
		}
		if f.Exited {
			s.EndedAt = new(f.LastAt)
		}
		if f.LastError != "" {
			s.Error = new(f.LastError)
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
