package github

// THROWAWAY evidence probe (not committed): peak-heap comparison of the deleted
// collect-all path vs the streaming path over the identical paged stub.
// Run: go test ./internal/github -run TestThrowawayPeak -v -count=1
// Delete this file after collecting evidence.

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oh-my-omp/pm-council/internal/markers"
)

func throwawayPeak(fn func()) uint64 {
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

func TestThrowawayPeak(t *testing.T) {
	const seed = 7
	// collect-all over the stub's pages (mirrors deleted IssueComments behavior)
	c1, close1 := stubPagedComments(seed)
	defer close1()
	var nAll int
	var retained []Comment // mirrors the deleted IssueComments: all pages held at once
	peakAll := throwawayPeak(func() {
		nAll = 0
		retained = nil
		_ = eachPage[Comment](context.Background(), c1.rest, "repos/o/pm/issues/1/comments?per_page=100", func(page []Comment) error {
			retained = append(retained, page...)
			return nil
		})
		for _, cm := range retained {
			if _, err := markers.Parse(cm.Body); err == nil {
				nAll++
			}
		}
	})
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	c2, close2 := stubPagedComments(seed)
	defer close2()
	var nStream int
	peakStream := throwawayPeak(func() {
		nStream = 0
		_ = c2.EachComment(context.Background(), "o/pm", 1, func(body string, _ time.Time) error {
			if _, err := markers.Parse(body); err == nil {
				nStream++
			}
			return nil
		})
	})
	t.Logf("THROWAWAY pages=%d perPage=%d padBytes=%d peakCollect=%.2fMB peakStream=%.2fMB",
		pagedPages, pagedPerPage, pagedPadBytes, float64(peakAll)/(1<<20), float64(peakStream)/(1<<20))
}
