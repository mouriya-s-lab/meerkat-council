package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyExample copies config.example into a temp dir and returns its path.
func copyExample(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "config.example")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestLoadExample(t *testing.T) {
	cfg, err := Load(copyExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Project.Fields.Status.Options.InProgress; got != "In Progress" {
		t.Errorf("status inProgress = %q", got)
	}
	if len(cfg.Seats) != 4 || !cfg.Seats[2].Has(RightVeto) {
		t.Errorf("seats = %+v", cfg.Seats)
	}
	if cfg.Runtime.Daemon.RebuildIntervalSeconds != 60 || cfg.Council.Agenda.Prefix != "D" {
		t.Errorf("runtime/council = %+v %+v", cfg.Runtime.Daemon, cfg.Council)
	}
}

func TestLoadErrors(t *testing.T) {
	for _, tc := range []struct {
		name, file, old, new, want string
	}{
		{"missing prefix", "council/council.yaml", "prefix: D, ", "", "council/council.yaml: agenda.prefix: missing"},
		{"unknown key", "council/council.yaml", "maxConcurrentPanels", "maxPanels", "council/council.yaml: maxPanels"},
		{"bad right", "council/seats.yaml", "rights: [vote, veto]", "rights: [vote, shout]", "council/seats.yaml: seats[2].rights[1]"},
		{"missing option", "shared/project.yaml", "done: Done, ", "", "shared/project.yaml: fields.status.options.done: missing"},
		{"absolute prompt", "council/seats.yaml", "prompt: omp/prompts/Argon.md", "prompt: /tmp/Argon.md", "council/seats.yaml: seats[2].prompt"},
		{"zero interval", "council/runtime.yaml", "rebuildIntervalSeconds: 60", "rebuildIntervalSeconds: 0", "council/runtime.yaml: daemon.rebuildIntervalSeconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyExample(t)
			path := filepath.Join(root, tc.file)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), tc.old) {
				t.Fatalf("%s does not contain %q", tc.file, tc.old)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(b), tc.old, tc.new, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err = Load(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
