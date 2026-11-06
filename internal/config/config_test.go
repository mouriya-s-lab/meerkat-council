package config

import (
	"os"
	"path/filepath"
	"reflect"
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
	if cfg.Runtime.Daemon.RebuildIntervalSeconds != 60 || cfg.Council.Agenda.Prefix != "D" || cfg.Council.Resume != ResumeFork {
		t.Errorf("runtime/council = %+v %+v", cfg.Runtime.Daemon, cfg.Council)
	}
	if !filepath.IsAbs(cfg.Seats[0].Prompt) || cfg.DraftSeat().ID != "Coordination" {
		t.Errorf("seat prompt %q, draft seat %q", cfg.Seats[0].Prompt, cfg.DraftSeat().ID)
	}
	if p, ok := cfg.ProxyReader("Argon"); !ok || !reflect.DeepEqual(p.Content, []Content{ContentDesign}) || p.Others == nil {
		t.Errorf("Argon proxy = %+v %v", p, ok)
	}
	out, err := cfg.Goals.Draft.RenderOpening(OpeningData{Ref: "o/r#7", Agenda: "D7", Round: 1, Version: 1,
		Escalation: &EscalationData{Issue: "o/c#3", Ask: 42, AskText: "which flag?"}})
	if err != nil || !strings.Contains(out, "o/c#3") || !strings.Contains(out, "which flag?") || !strings.Contains(out, "D7-") {
		t.Errorf("draft opening = %q, %v", out, err)
	}
	if out, err := cfg.Goals.Vote.RenderResume(OpeningData{Ref: "o/r#7", Version: 1}); err != nil || !strings.Contains(out, "o/r#7") {
		t.Errorf("vote resume = %q, %v", out, err)
	}
}

func TestLoadErrors(t *testing.T) {
	for _, tc := range []struct {
		name, file, old, new, want string
	}{
		{"missing prefix", "council/council.yaml", "prefix: D, ", "", "council/council.yaml: agenda.prefix: missing"},
		{"unknown key", "council/council.yaml", "maxConcurrentPanels", "maxPanels", "council/council.yaml: maxPanels"},
		{"bad right", "council/seats.yaml", "rights: [vote, veto]", "rights: [vote, shout]", "council/seats.yaml: seats[2].rights[1]"},
		{"absolute prompt", "council/seats.yaml", "prompt: omp/prompts/Argon.md", "prompt: /tmp/Argon.md", "council/seats.yaml: seats[2].prompt"},
		{"missing prompt file", "council/seats.yaml", "prompt: omp/prompts/Argon.md", "prompt: omp/prompts/Nobody.md", "council/seats.yaml: seats[2].prompt: stat"},
		{"two drafters", "council/seats.yaml", "rights: [vote]\n", "rights: [vote, draft]\n", "council/seats.yaml: seats: exactly one seat must have the draft right, 2 have"},
		{"zero interval", "council/runtime.yaml", "rebuildIntervalSeconds: 60", "rebuildIntervalSeconds: 0", "council/runtime.yaml: daemon.rebuildIntervalSeconds"},
		{"missing resume", "council/council.yaml", "resume: fork\n", "", "council/council.yaml: resume: missing"},
		{"bad resume", "council/council.yaml", "resume: fork", "resume: restart", `council/council.yaml: resume: "restart" is not one of fork, fresh`},
		{"foreign goal tool", "council/goals.yaml", "goalTools: [pm_vote]", "goalTools: [pm_draft]", `council/goals.yaml: vote.goalTools[0]: "pm_draft" is not a vote goal tool`},
		{"unknown template field", "council/goals.yaml", "{{.Ref}}（议程编号 {{.Agenda}}", "{{.Issue}}（议程编号 {{.Agenda}}", "council/goals.yaml: draft.opening"},
		{"unguarded escalation", "council/goals.yaml", "{{if .Escalation}}", "{{.Escalation.Issue}}{{if .Escalation}}", "council/goals.yaml: draft.opening"},
		{"missing continue", "council/goals.yaml", "  continue: 你还没有成功调用 pm_vote。审查决策表的每一行，调用 pm_vote 交出本席位的票；若工具返回错误，按错误修正后重试。", `  continue: ""`, "council/goals.yaml: vote.continue: missing"},
		{"unknown proxy seat", "council/proxy-readers.yaml", "seat: Argon", "seat: Nobody", `council/proxy-readers.yaml: seats[2].seat: "Nobody" is not a seat`},
		{"voter without proxy entry", "council/proxy-readers.yaml", "seat: Argon", "seat: LeadMentor", `council/proxy-readers.yaml: seats: seat "Argon" drafts or votes and has no entry`},
		{"bad content", "council/proxy-readers.yaml", "content: [design]", "content: [web]", `council/proxy-readers.yaml: seats[2].content[0]: "web" is not one of code, design, omp`},
		{"missing others", "council/proxy-readers.yaml", "    others: []\n    content: [design]", "    content: [design]", "council/proxy-readers.yaml: seats[2].others: missing"},
		{"missing option", "shared/project.yaml", "done: Done, ", "", "shared/project.yaml: fields.status.options.done: missing"},
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
