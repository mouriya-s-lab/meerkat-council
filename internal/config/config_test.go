package config

import (
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	if cfg.Tally.Threshold.Of(4) != 3 || cfg.Tally.Arbitration.Seat != "LeadMentor" || cfg.Tally.MaxRounds != 2 || cfg.Council.Archive != nil {
		t.Errorf("tally = %+v, archive = %+v", cfg.Tally, cfg.Council.Archive)
	}
	if !reflect.DeepEqual(cfg.Goals.Of(KindArbitrate).GoalTools, []string{ToolRuling}) || !reflect.DeepEqual(KindRevise.PluginTools(), []string{ToolReadIssue, ToolProxyRead, ToolDraft}) {
		t.Errorf("arbitrate goal tools %v, revise plugin tools %v", cfg.Goals.Arbitrate.GoalTools, KindRevise.PluginTools())
	}
	row := sampleRow()
	out, err = cfg.Goals.Arbitrate.RenderOpening(OpeningData{Ref: "o/r#7", Agenda: "D7", Version: 1, Categories: cfg.Tally.Categories,
		Disputes: []DisputeData{{Row: row, Seat: "Argon", Citation: "o/r#2"}}})
	if err != nil || !strings.Contains(out, row.ID) || !strings.Contains(out, "o/r#2") || !strings.Contains(out, cfg.Tally.Categories[1]) {
		t.Errorf("arbitrate opening = %q, %v", out, err)
	}
	out, err = cfg.Goals.Revise.RenderOpening(OpeningData{Ref: "o/r#7", Round: 2, MaxRounds: 2, Version: 2,
		Counterexamples: []CounterexampleData{{Seat: "ClaudeHigh", Row: row.ID, Counterexample: "breaks count"}}})
	if err != nil || !strings.Contains(out, "breaks count") || !strings.Contains(out, "ClaudeHigh") {
		t.Errorf("revise opening = %q, %v", out, err)
	}

	link, err := cfg.Council.LaunchLink.Link("o/c", 5, "/Users/x y/c (1)")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme+"://"+u.Host != "http://127.0.0.1:47591" || u.Path != "/o/c/issues/5" || strings.ContainsAny(link, " ()") {
		t.Fatalf("link %q: %v", link, err)
	}
	if spec := u.Query().Get("spec"); spec != `{"cwd":"/Users/x y/c (1)","frontend":{"kind":"tern"}}` {
		t.Errorf("spec = %s", spec)
	}

	d := ImplIssueData{Ref: "o/r#7", Agenda: "D7", Row: row}
	title, err := cfg.Council.ImplIssue.RenderTitle(d)
	if err != nil || title != row.Deliver.Title {
		t.Errorf("impl title = %q, %v", title, err)
	}
	created, _ := cfg.Council.ImplIssue.RenderBody(d)
	d.Link, d.Replaces = link, &ReplacesData{Old: "o/c#3", Ask: 42}
	edited, err := cfg.Council.ImplIssue.RenderBody(d)
	if err != nil || strings.Contains(created, "47591") || !strings.Contains(edited, link) || !strings.Contains(edited, "o/c#3") || !strings.Contains(edited, "o/r#7") {
		t.Errorf("impl body created %q edited %q, %v", created, edited, err)
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
		{"zero shutdown timeout", "council/runtime.yaml", "shutdownTimeoutSeconds: 15", "shutdownTimeoutSeconds: 0", "council/runtime.yaml: daemon.shutdownTimeoutSeconds"},
		{"missing session timeout", "council/runtime.yaml", "sessionTimeoutSeconds: 300\n", "", "council/runtime.yaml: sessionTimeoutSeconds"},
		{"zero session timeout", "council/runtime.yaml", "sessionTimeoutSeconds: 300", "sessionTimeoutSeconds: 0", "council/runtime.yaml: sessionTimeoutSeconds"},
		{"bad resume", "council/council.yaml", "resume: fork", "resume: restart", `council/council.yaml: resume: "restart" is not one of fork, fresh`},
		{"foreign goal tool", "council/goals.yaml", "goalTools: [meerkat_vote]", "goalTools: [meerkat_draft]", `council/goals.yaml: vote.goalTools[0]: "meerkat_draft" is not a vote goal tool`},
		{"removed goal tool", "council/goals.yaml", "goalTools: [meerkat_draft]", "goalTools: [meerkat_draft, meerkat_block]", `council/goals.yaml: draft.goalTools[1]: "meerkat_block" is not a draft goal tool`},
		{"unknown template field", "council/goals.yaml", "{{.Ref}}（议程编号 {{.Agenda}}", "{{.Issue}}（议程编号 {{.Agenda}}", "council/goals.yaml: draft.opening"},
		{"unguarded escalation", "council/goals.yaml", "{{if .Escalation}}", "{{.Escalation.Issue}}{{if .Escalation}}", "council/goals.yaml: draft.opening"},
		{"missing continue", "council/goals.yaml", "  continue: 你还没有成功调用 meerkat_vote。审查决策表的每一行，调用 meerkat_vote 交出本席位的票；若工具返回错误，按错误修正后重试。", `  continue: ""`, "council/goals.yaml: vote.continue: missing"},
		{"unknown proxy seat", "council/proxy-readers.yaml", "seat: Argon", "seat: Nobody", `council/proxy-readers.yaml: seats[2].seat: "Nobody" is not a seat`},
		{"voter without proxy entry", "council/proxy-readers.yaml", "seat: Argon", "seat: LeadMentor", `council/proxy-readers.yaml: seats: seat "Argon" drafts or votes and has no entry`},
		{"bad content", "council/proxy-readers.yaml", "content: [design]", "content: [web]", `council/proxy-readers.yaml: seats[2].content[0]: "web" is not one of code, design, omp`},
		{"missing others", "council/proxy-readers.yaml", "    others: []\n    content: [design]", "    content: [design]", "council/proxy-readers.yaml: seats[2].others: missing"},
		{"missing option", "shared/project.yaml", "done: Done, ", "", "shared/project.yaml: fields.status.options.done: missing"},
		{"veto seat without veto right", "council/tally.yaml", "seats: [Argon]", "seats: [ClaudeHigh]", `council/tally.yaml: veto.seats[0]: seat "ClaudeHigh" does not have the veto right`},
		{"unknown arbitration seat", "council/tally.yaml", "seat: LeadMentor", "seat: Nobody", `council/tally.yaml: arbitration.seat: "Nobody" is not a seat`},
		{"arbitration seat without right", "council/tally.yaml", "seat: LeadMentor", "seat: Argon", `council/tally.yaml: arbitration.seat: seat "Argon" does not have the arbitrate right`},
		{"veto category not a category", "council/tally.yaml", "categories: [意图]\n", "categories: [安全]\n", `council/tally.yaml: veto.categories[0]: "安全" is not one of categories`},
		{"threshold above one", "council/tally.yaml", "numerator: 3", "numerator: 5", "council/tally.yaml: threshold: numerator 5 exceeds denominator 4"},
		{"missing maxRounds", "council/tally.yaml", "maxRounds: 2\n", "", "council/tally.yaml: maxRounds"},
		{"missing veto seats", "council/tally.yaml", "  seats: [Argon]\n", "", "council/tally.yaml: veto.seats: missing"},
		{"unknown tally key", "council/tally.yaml", "maxRounds", "rounds", "council/tally.yaml: rounds"},
		{"unguarded replaces", "council/council.yaml", "{{- if .Replaces}}", "{{.Replaces.Old}}{{- if .Replaces}}", "council/council.yaml: implIssue.body"},
		{"bad launch base", "council/council.yaml", "base: http://127.0.0.1:47591", "base: 127.0.0.1:47591", `council/council.yaml: launchLink.base: "127.0.0.1:47591" is not an http(s) URL`},
		{"missing frontend", "council/council.yaml", "  frontend: { kind: tern }\n", "", "council/council.yaml: launchLink.frontend.kind: missing"},
		{"unknown reply field", "council/council.yaml", "{{.Text}}", "{{.Settle}}", "council/council.yaml: reply.body"},
		{"archive outside the repository", "council/council.yaml", "archive: null", "archive: { decisions: ../d, votes: votes }", `council/council.yaml: archive.decisions: "../d" is not a clean relative directory`},
		{"arbitrate goal tool", "council/goals.yaml", "goalTools: [meerkat_ruling]", "goalTools: [meerkat_vote]", `council/goals.yaml: arbitrate.goalTools[0]: "meerkat_vote" is not a arbitrate goal tool`},
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

// exampleFiles are the files of config.example that the loader reads, by path relative to the root.
var exampleFiles = []string{
	"council/runtime.yaml",
	"council/council.yaml",
	"council/seats.yaml",
	"council/goals.yaml",
	"council/tally.yaml",
	"council/proxy-readers.yaml",
	"council/omp/prompts/Argon.md",
	"council/omp/prompts/ClaudeHigh.md",
	"council/omp/prompts/Coordination.md",
	"council/omp/prompts/LeadMentor.md",
	"shared/repos.yaml",
	"shared/project.yaml",
}

func readFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, rel := range exampleFiles {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		files[rel] = string(b)
	}
	return files
}

func TestBlobSHA(t *testing.T) {
	// `printf 'hello\n' | git hash-object --stdin`
	if got := blobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("blobSHA = %s", got)
	}
}

func TestLoadedFiles(t *testing.T) {
	root := copyExample(t)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range cfg.Loaded.Files {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if f.SHA != blobSHA(b) {
			t.Errorf("%s sha = %s, want %s", f.Path, f.SHA, blobSHA(b))
		}
		got = append(got, f.Path)
	}
	want := slices.Clone(exampleFiles)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded files = %v, want %v", got, want)
	}
	if cfg.Loaded.LoadedAt.IsZero() {
		t.Error("LoadedAt not set")
	}
}

func TestCheck(t *testing.T) {
	valid := readFiles(t, copyExample(t))
	for _, tc := range []struct {
		name   string
		mutate func(files map[string]string)
		// file and key of the one expected issue; empty file for a valid set
		file, key string
	}{
		{name: "valid set", mutate: func(map[string]string) {}},
		{"unknown key", func(f map[string]string) {
			f["council/council.yaml"] = strings.Replace(f["council/council.yaml"], "maxConcurrentPanels", "maxPanels", 1)
		}, "council/council.yaml", "maxPanels"},
		{"missing key", func(f map[string]string) {
			f["council/council.yaml"] = strings.Replace(f["council/council.yaml"], "prefix: D, ", "", 1)
		}, "council/council.yaml", "agenda.prefix"},
		{"missing referenced file", func(f map[string]string) {
			delete(f, "council/omp/prompts/Argon.md")
		}, "council/seats.yaml", "seats[2].prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := maps.Clone(valid)
			tc.mutate(files)
			issues, err := Check(files)
			if err != nil {
				t.Fatal(err)
			}
			if tc.file == "" {
				if len(issues) != 0 {
					t.Fatalf("issues = %v, want none", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].File != tc.file || issues[0].Key != tc.key {
				t.Fatalf("issues = %v, want one at %s: %s", issues, tc.file, tc.key)
			}
		})
	}
}
