package github

// Large paged load: the stub serves P pages x C comments of ~8 KiB bodies
// chained by Link rel="next", exactly like GitHub pagination. The stub
// regenerates bodies per request, so nothing but the consumer's own retention
// lives past a page.

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

	"github.com/mouriya-s-lab/meerkat-council/internal/markers"
)

const (
	pagedPages       = 20
	pagedPerPage     = 100
	pagedPadBytes    = 8 << 10
	pagedMarkerEvery = 4
)

func pagedBody(seed, i int) string {
	// Deterministic 8 KiB body from a small seed: no big fixture lives in the
	// test process; the stub rebuilds the same bytes on every request.
	var sb strings.Builder
	sb.Grow(pagedPadBytes)
	fmt.Fprintf(&sb, "comment %d seed %d ", i, seed)
	for sb.Len() < pagedPadBytes-128 {
		fmt.Fprintf(&sb, "%08x", (seed*2654435761+i*40503+sb.Len()*97)&0xffffffff)
	}
	pad := sb.String()[:pagedPadBytes-128]
	if i%pagedMarkerEvery == 2 {
		return fmt.Sprintf("%s\nplain human discussion, no marker", pad)
	}
	return fmt.Sprintf("%s\n<!-- pm:superseded {\"v\":1,\"by\":\"o/pm#9\"} -->", pad)
}

func stubPagedComments(seed int) (*Client, func()) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := 1
		if q := r.URL.Query().Get("page"); q != "" {
			fmt.Sscanf(q, "%d", &page)
		}
		var sb strings.Builder
		sb.WriteByte('[')
		for c := 0; c < pagedPerPage; c++ {
			i := (page-1)*pagedPerPage + c
			if c > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"body":%q,"created_at":"2026-11-07T12:00:01Z"}`, pagedBody(seed, i))
		}
		sb.WriteByte(']')
		if page < pagedPages {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/pm/issues/1/comments?per_page=100&page=%d>; rel="next"`, srv.URL, page+1))
		}
		w.Write([]byte(sb.String()))
	}))
	u, err := url.Parse(srv.URL)
	if err != nil {
		panic(err)
	}
	rest, err := api.NewRESTClient(api.ClientOptions{
		AuthToken: "test",
		Host:      u.Hostname(),
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
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
		panic(err)
	}
	return &Client{rest: rest}, srv.Close
}

// Streaming over the paged stub visits every comment oldest-first and parses
// the same markers CommentMarkers yields: the two paths agree marker for
// marker, and the count matches the analytically expected value (every 4th
// comment carries no marker).
func TestPagedCommentsAgree(t *testing.T) {
	const seed = 7
	c, close := stubPagedComments(seed)
	defer close()
	var streamed []string
	if err := c.EachComment(context.Background(), "o/pm", 1, func(body string, _ time.Time) error {
		streamed = append(streamed, body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != pagedPages*pagedPerPage {
		t.Fatalf("comments = %d, want %d", len(streamed), pagedPages*pagedPerPage)
	}
	for i := 1; i < len(streamed); i++ {
		if !strings.Contains(streamed[i], "comment") {
			t.Fatalf("comment %d out of order or corrupt", i)
		}
	}
	nParse := 0
	for _, b := range streamed {
		if _, err := markers.Parse(b); err == nil {
			nParse++
		}
	}
	nMarkers := 0
	if err := c.CommentMarkers(context.Background(), "o/pm", 1, func(markers.Marker) error {
		nMarkers++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if nParse != nMarkers {
		t.Fatalf("EachComment markers %d, CommentMarkers %d", nParse, nMarkers)
	}
	if want := pagedPages * pagedPerPage * 3 / 4; nMarkers != want {
		t.Fatalf("markers = %d, want %d", nMarkers, want)
	}
}
