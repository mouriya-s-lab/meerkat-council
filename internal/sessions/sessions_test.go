package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

const done = `{"type":"title","v":1,"title":""}
{"type":"session","version":3,"id":"s-done","timestamp":"2026-11-06T21:30:15.000Z","cwd":"/x"}
{"type":"custom","customType":"pm-meta","data":{"kind":"vote","seat":"Argon","model":"m/a","ref":"o/pm#1"},"id":"a","timestamp":"2026-11-06T21:30:15.100Z"}
{"type":"message","id":"b","timestamp":"2026-11-06T21:30:16.000Z","message":{"role":"assistant","content":[]}}
{"type":"custom","customType":"session_exit","data":{"reason":"dispose","kind":"normal"},"id":"c","timestamp":"2026-11-06T21:30:20.000Z"}
`

const failed = `{"type":"session","version":3,"id":"s-failed","timestamp":"2026-11-06T22:00:00.000Z","cwd":"/x"}
{"type":"message","id":"b","timestamp":"2026-11-06T22:00:01.000Z","message":{"role":"assistant","errorMessage":"429 rate limited"}}
{"type":"custom","customType":"session_exit","data":{"reason":"dispose","kind":"normal"},"id":"c","timestamp":"2026-11-06T22:00:02.000Z"}
`

// A fork copies the base's history, session_exit included, before its own header timestamp.
const running = `{"type":"session","version":3,"id":"s-running","timestamp":"2026-11-06T23:00:00.000Z","cwd":"/x"}
{"type":"custom","customType":"session_exit","data":{"reason":"dispose","kind":"normal"},"id":"c","timestamp":"2026-11-06T21:30:20.000Z"}
{"type":"message","id":"b","timestamp":"2026-11-06T23:00:01.000Z","message":{"role":"user","content":[]}}
`

func TestList(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"a/done.jsonl": done, "failed.jsonl": failed, "running.jsonl": running, "other.jsonl": `{"type":"x"}` + "\n"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list, err := List(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %+v", list)
	}
	r, f, d := list[0], list[1], list[2]
	if r.ID != "s-running" || r.State != StateRunning || r.EndedAt != nil {
		t.Errorf("running = %+v", r)
	}
	if f.ID != "s-failed" || f.State != StateFailed || f.Error == nil || *f.Error != "429 rate limited" {
		t.Errorf("failed = %+v", f)
	}
	if d.ID != "s-done" || d.State != StateDone || d.Kind != "vote" || d.Label != "vote Argon o/pm#1" ||
		d.Ref == nil || *d.Ref != "o/pm#1" || d.EndedAt == nil || d.Model != "m/a" {
		t.Errorf("done = %+v", d)
	}
	if list, _ := List(dir, 1); len(list) != 1 {
		t.Errorf("limit 1 = %d rows", len(list))
	}
	if list, err := List(filepath.Join(dir, "missing"), 5); err != nil || len(list) != 0 || list == nil {
		t.Errorf("missing dir = %v, %v", list, err)
	}
	if p, ok, err := FindFile(dir, "s-done"); err != nil || !ok || filepath.Base(p) != "done.jsonl" {
		t.Errorf("FindFile = %q %v %v", p, ok, err)
	}
}
