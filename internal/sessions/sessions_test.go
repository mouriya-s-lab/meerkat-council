package sessions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oh-my-omp/pm-council/internal/config"
)

const done = `{"type":"title","v":1,"title":""}
{"type":"session","version":3,"id":"s-done","timestamp":"2026-11-06T21:30:15.000Z","cwd":"/x"}
{"type":"custom","customType":"pm-meta","data":{"kind":"vote","seat":"Argon","model":"m/a","ref":"o/pm#1","round":1,"version":1},"id":"a","timestamp":"2026-11-06T21:30:15.100Z"}
{"type":"message","id":"b","timestamp":"2026-11-06T21:30:16.000Z","message":{"role":"toolResult","toolName":"pm_vote","isError":true}}
{"type":"message","id":"c","timestamp":"2026-11-06T21:30:17.000Z","message":{"role":"toolResult","toolName":"pm_vote","isError":false}}
`

const failed = `{"type":"session","version":3,"id":"s-failed","timestamp":"2026-11-06T22:00:00.000Z","cwd":"/x"}
{"type":"custom","customType":"pm-meta","data":{"kind":"draft","seat":"Coordination","model":"m/a","ref":"o/pm#2","round":1,"version":1},"id":"a","timestamp":"2026-11-06T22:00:00.100Z"}
{"type":"message","id":"b","timestamp":"2026-11-06T22:00:01.000Z","message":{"role":"assistant","errorMessage":"429 rate limited"}}
`

const noGoal = `{"type":"session","version":3,"id":"s-nogoal","timestamp":"2026-11-06T22:30:00.000Z","cwd":"/x"}
{"type":"custom","customType":"pm-meta","data":{"kind":"draft","seat":"Coordination","model":"m/a","ref":"o/pm#2","round":1,"version":1},"id":"a","timestamp":"2026-11-06T22:30:00.100Z"}
{"type":"message","id":"b","timestamp":"2026-11-06T22:30:01.000Z","message":{"role":"toolResult","toolName":"pm_read_issue","isError":false}}
`

// A fork copies its parent's history, the goal tool result included, before its own header
// timestamp; that copy does not make the fork done.
const fork = `{"type":"session","version":3,"id":"s-fork","parentSession":"s-done","timestamp":"2026-11-06T23:00:00.000Z","cwd":"/x"}
{"type":"custom","customType":"pm-meta","data":{"kind":"vote","seat":"Argon","model":"m/a","ref":"o/pm#1","round":1,"version":1},"id":"a","timestamp":"2026-11-06T21:30:15.100Z"}
{"type":"message","id":"c","timestamp":"2026-11-06T21:30:17.000Z","message":{"role":"toolResult","toolName":"pm_vote","isError":false}}
{"type":"message","id":"d","timestamp":"2026-11-06T23:00:01.000Z","message":{"role":"user","content":[]}}
`

func TestList(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"1/done.jsonl": done, "2/failed.jsonl": failed, "2/nogoal.jsonl": noGoal, "1/fork.jsonl": fork,
		"other.jsonl": `{"type":"x"}` + "\n", "1/context/vote-Argon-r1-v1.json": "{}",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goals := config.Goals{Draft: config.Goal{GoalTools: []string{"pm_draft", "pm_block"}}, Vote: config.Goal{GoalTools: []string{"pm_vote"}}}
	reg := NewRegistry()
	reg.failures["s-failed"] = "omp exited"

	list, err := reg.List(dir, goals, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("list = %+v", list)
	}
	fk, ng, f, d := list[0], list[1], list[2], list[3]
	if fk.ID != "s-fork" || fk.State != StateFailed || fk.Parent == nil || *fk.Parent != "s-done" || *fk.Error != "ended without a goal tool" {
		t.Errorf("fork = %+v", fk)
	}
	if ng.State != StateFailed || ng.Error == nil || *ng.Error != "ended without a goal tool" || ng.Parent != nil {
		t.Errorf("no goal = %+v", ng)
	}
	if f.ID != "s-failed" || f.State != StateFailed || f.Error == nil || *f.Error != "omp exited" {
		t.Errorf("failed = %+v", f)
	}
	if d.ID != "s-done" || d.State != StateDone || d.Kind != config.KindVote || d.Label != "vote Argon o/pm#1" ||
		d.Ref == nil || *d.Ref != "o/pm#1" || d.EndedAt == nil || d.Model != "m/a" || d.Error != nil || d.Parent != nil {
		t.Errorf("done = %+v", d)
	}

	// The registry's reason wins over the file; without it the last failed assistant message.
	delete(reg.failures, "s-failed")
	reg.live["s-fork"] = true
	list, _ = reg.List(dir, goals, 10)
	if list[0].State != StateRunning || list[0].EndedAt != nil || list[0].Error != nil {
		t.Errorf("live fork = %+v", list[0])
	}
	if *list[2].Error != "429 rate limited" {
		t.Errorf("failed without registry reason = %+v", list[2])
	}
	if list, _ := reg.List(dir, goals, 1); len(list) != 1 {
		t.Errorf("limit 1 = %d rows", len(list))
	}
	if list, err := reg.List(filepath.Join(dir, "missing"), goals, 5); err != nil || len(list) != 0 || list == nil {
		t.Errorf("missing dir = %v, %v", list, err)
	}
	if p, ok, err := FindFile(dir, "s-done"); err != nil || !ok || filepath.Base(p) != "done.jsonl" {
		t.Errorf("FindFile = %q %v %v", p, ok, err)
	}
}
