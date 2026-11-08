package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mouriya-s-lab/meerkat-council/internal/api"
)

var secret = []byte("s3cret")

func sign(body string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := sign(string(body))
	for _, tc := range []struct {
		name, header string
		body         []byte
		want         bool
	}{
		{"good", good, body, true},
		{"body changed", good, []byte(`{"a":2}`), false},
		{"missing", "", body, false},
		{"sha1 prefix", "sha1=" + strings.TrimPrefix(good, "sha256="), body, false},
		{"not hex", "sha256=zz", body, false},
		{"other secret", "sha256=" + strings.Repeat("0", 64), body, false},
	} {
		if got := ValidSignature(secret, tc.body, tc.header); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestWebhook(t *testing.T) {
	var mu sync.Mutex
	var got []api.Event
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" || r.Method != http.MethodPost {
			t.Errorf("daemon got %s %s", r.Method, r.URL.Path)
		}
		var ev api.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Error(err)
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer daemon.Close()
	h := New(secret, "o/agenda", daemon.URL+"/").Handler()

	issues := `{"action":"opened","issue":{"number":7},"repository":{"full_name":"o/agenda"}}`
	for _, tc := range []struct {
		name, event, body, sig string
		want                   int
		forward                *api.Event
	}{
		{"issues forwarded", "issues", issues, sign(issues), 202, &api.Event{Issue: "o/agenda#7", Event: "issues", Action: "opened"}},
		{"tampered body", "issues", strings.Replace(issues, "7", "8", 1), sign(issues), 401, nil},
		{"missing signature", "issues", issues, "", 401, nil},
		{"ping", "ping", `{"zen":"x"}`, sign(`{"zen":"x"}`), 200, nil},
		{"other repo", "issues", `{"action":"opened","issue":{"number":7},"repository":{"full_name":"o/other"}}`, sign(`{"action":"opened","issue":{"number":7},"repository":{"full_name":"o/other"}}`), 202, nil},
		{"other event", "push", `{"repository":{"full_name":"o/agenda"}}`, sign(`{"repository":{"full_name":"o/agenda"}}`), 202, nil},
		{"comment", "issue_comment", `{"action":"created","issue":{"number":3},"repository":{"full_name":"o/agenda"}}`, sign(`{"action":"created","issue":{"number":3},"repository":{"full_name":"o/agenda"}}`), 202, &api.Event{Issue: "o/agenda#3", Event: "issue_comment", Action: "created"}},
		{"dependency uses blocked_issue", "issue_dependencies", `{"action":"blocked_by_added","blocked_issue":{"number":5},"blocking_issue":{"number":4},"repository":{"full_name":"o/agenda"}}`, sign(`{"action":"blocked_by_added","blocked_issue":{"number":5},"blocking_issue":{"number":4},"repository":{"full_name":"o/agenda"}}`), 202, &api.Event{Issue: "o/agenda#5", Event: "issue_dependencies", Action: "blocked_by_added"}},
		{"sub issue uses parent_issue", "sub_issues", `{"action":"sub_issue_added","parent_issue":{"number":2},"sub_issue":{"number":9},"repository":{"full_name":"o/agenda"}}`, sign(`{"action":"sub_issue_added","parent_issue":{"number":2},"sub_issue":{"number":9},"repository":{"full_name":"o/agenda"}}`), 202, &api.Event{Issue: "o/agenda#2", Event: "sub_issues", Action: "sub_issue_added"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			got = nil
			mu.Unlock()
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(tc.body))
			req.Header.Set("X-GitHub-Event", tc.event)
			if tc.sig != "" {
				req.Header.Set("X-Hub-Signature-256", tc.sig)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case tc.forward == nil && len(got) != 0:
				t.Fatalf("forwarded %v, want nothing", got)
			case tc.forward != nil && (len(got) != 1 || got[0] != *tc.forward):
				t.Fatalf("forwarded %v, want %v", got, *tc.forward)
			}
		})
	}
}

func TestWebhookDaemonDown(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	daemon.Close()
	h := New(secret, "o/agenda", daemon.URL).Handler()
	body := `{"action":"opened","issue":{"number":7},"repository":{"full_name":"o/agenda"}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
