package github

// Large paged load: the stub serves P pages x C comments of ~8 KiB bodies
// chained by Link rel="next", exactly like GitHub pagination. Two consumers
// run over the identical stub: the old collect-all path (pages/IssueComments,
// one []Comment for all pages) and the new streaming path (EachComment, one
// page at a time). Peak heap is sampled while each consumer runs, so the delta
// is the retention difference, not fixture storage (the stub regenerates
// bodies per request — nothing but the consumer's own retention lives on).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/oh-my-omp/pm-council/internal/markers"
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

func samplePeakHeap(fn func()) uint64 {
	done := make(chan struct{})
	var peak atomic.Uint64
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			default:
				runtime.ReadMemStats(&m)
				for {
					p := peak.Load()
					if uint64(m.HeapInuse) <= p || peak.CompareAndSwap(p, uint64(m.HeapInuse)) {
						break
					}
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	fn()
	close(done)
	return peak.Load()
}

// Both consumers parse the same marker lines; only the retention differs.
func consumeAll(c *Client) (int, error) {
	cs, err := c.IssueComments(context.Background(), "o/pm", 1)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, cm := range cs {
		if _, err := markers.Parse(cm.Body); err == nil {
			n++
		}
	}
	return n, nil
}

func consumeStream(c *Client) (int, error) {
	n := 0
	err := c.EachComment(context.Background(), "o/pm", 1, func(body string, _ time.Time) error {
		if _, err := markers.Parse(body); err == nil {
			n++
		}
		return nil
	})
	return n, err
}

func TestPagedPeakHeap(t *testing.T) {
	const seed = 7
	c1, close1 := stubPagedComments(seed)
	defer close1()
	var nAll int
	var err error
	peakAll := samplePeakHeap(func() {
		nAll, err = consumeAll(c1)
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	c2, close2 := stubPagedComments(seed)
	defer close2()
	var nStream int
	peakStream := samplePeakHeap(func() {
		nStream, err = consumeStream(c2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if nAll != nStream {
		t.Fatalf("marker counts differ: collect-all %d, streaming %d", nAll, nStream)
	}
	total := pagedPages * pagedPerPage * pagedPadBytes
	t.Logf("PAGED pages=%d perPage=%d padBytes=%d totalBodies=%.2fMB markers=%d peakCollect=%.2fMB peakStream=%.2fMB",
		pagedPages, pagedPerPage, pagedPadBytes, float64(total)/(1<<20), nAll,
		float64(peakAll)/(1<<20), float64(peakStream)/(1<<20))
	if peakStream >= peakAll {
		t.Fatalf("no peak reduction: collect-all %.2fMB, streaming %.2fMB",
			float64(peakAll)/(1<<20), float64(peakStream)/(1<<20))
	}
}
