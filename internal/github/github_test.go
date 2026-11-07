package github

// Paginated streaming: EachComment visits every comment across pages oldest
// first and releases each page before fetching the next. The stub serves two
// pages; the Link header chains them exactly like GitHub's rel="next".

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/oh-my-omp/pm-council/internal/markers"
)

func stubComments(t *testing.T, bodies []string) (*Client, func()) {
	t.Helper()
	page1 := `[{"body":%q,"created_at":"2026-11-07T12:00:01Z"},{"body":%q,"created_at":"2026-11-07T12:00:02Z"}]`
	_ = page1
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "page=2") {
			fmt.Fprintf(w, `[{"body":%q,"created_at":"2026-11-07T12:00:03Z"}]`, bodies[2])
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/pm/issues/1/comments?per_page=100&page=2>; rel="next"`, srv.URL))
		fmt.Fprintf(w, `[{"body":%q,"created_at":"2026-11-07T12:00:01Z"},{"body":%q,"created_at":"2026-11-07T12:00:02Z"}]`, bodies[0], bodies[1])
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := api.NewRESTClient(api.ClientOptions{
		AuthToken: "test",
		Host:      u.Hostname(),
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			// Rewrite the request at the stub server regardless of the
			// api.<host> prefix go-gh builds for non-localhost hosts.
			r2 := r.Clone(r.Context())
			r2.URL.Scheme = u.Scheme
			r2.URL.Host = u.Host
			r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v3")
			if r2.URL.Path == "" {
				r2.URL.Path = "/"
			}
			return http.DefaultTransport.RoundTrip(r2)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{rest: rest}, srv.Close
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEachCommentStreamsPages(t *testing.T) {
	bodies := []string{"first", "second <!-- pm:superseded {\"v\":1,\"by\":\"o/pm#9\"} -->", "third"}
	c, close := stubComments(t, bodies)
	defer close()
	var got []string
	var ats []time.Time
	if err := c.EachComment(context.Background(), "o/pm", 1, func(body string, at time.Time) error {
		got = append(got, body)
		ats = append(ats, at)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != bodies[0] || got[1] != bodies[1] || got[2] != bodies[2] {
		t.Fatalf("got %q, want %q", got, bodies)
	}
	for i := 1; i < len(ats); i++ {
		if !ats[i].After(ats[i-1]) {
			t.Fatalf("not oldest-first: %v", ats)
		}
	}
}

func TestCommentMarkersSkipsNonMarkers(t *testing.T) {
	bodies := []string{
		"plain discussion",
		"with marker\n\n<!-- pm:superseded {\"v\":1,\"by\":\"o/pm#9\"} -->",
		"<!-- pm:ask {\"v\":1} -->", // malformed for the council: skipped like Collect
	}
	c, close := stubComments(t, bodies)
	defer close()
	count := 0
	if err := c.CommentMarkers(context.Background(), "o/pm", 1, func(m markers.Marker) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("markers = %d, want 1", count)
	}
}
