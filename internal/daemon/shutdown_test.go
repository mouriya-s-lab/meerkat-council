package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/mouriya-s-lab/meerkat-council/internal/config"
	"github.com/mouriya-s-lab/meerkat-council/internal/github"
	"github.com/mouriya-s-lab/meerkat-council/internal/panel"
	"github.com/mouriya-s-lab/meerkat-council/internal/sessions"
)

// stubbornPanels ignores its context: it ends only when release is closed.
type stubbornPanels struct {
	started chan panel.Job
	release chan struct{}
	causes  chan error
}

func (f *stubbornPanels) Run(ctx context.Context, j panel.Job) {
	f.started <- j
	<-f.release
	f.causes <- context.Cause(ctx)
}

func shutdownConfig(timeoutSeconds int) *config.Config {
	cfg := testConfig()
	cfg.Council.MaxConcurrentPanels = 1
	cfg.Runtime.Daemon.ShutdownTimeoutSeconds = timeoutSeconds
	return cfg
}

func oneAgendaPanel() *fakeGitHub {
	return &fakeGitHub{items: []github.Item{
		{ID: "i1", Repo: "o/pm", Number: 1, Fields: map[string]string{"Kind": "Agenda", "Agenda": "D1", "Status": "Todo"}},
	}}
}

// A daemon stop cancels its running panels with the stop as the cause and the time of the cancel,
// and Run returns only after the panel has ended.
func TestShutdownCancelsPanelsWithTheStopTime(t *testing.T) {
	fp := &fakePanels{started: make(chan panel.Job, 8), ended: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, shutdownConfig(5), oneAgendaPanel(), project, fp)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(ran)
	}()
	<-fp.started

	before := time.Now().UTC()
	cancel()
	cause := <-fp.ended
	at, ok := sessions.ShutdownAt(cause)
	if !ok {
		t.Fatalf("cause = %v, want a daemon shutdown", cause)
	}
	if at.Before(before) || at.Sub(before) > time.Second {
		t.Errorf("cancel time %v, want within a second of %v", at, before)
	}
	<-ran
}

// A panel that ignores its cancel does not hold the daemon: Run returns after the shutdown timeout.
func TestShutdownReturnsAfterTimeoutWithStubbornPanel(t *testing.T) {
	sp := &stubbornPanels{started: make(chan panel.Job, 8), release: make(chan struct{}), causes: make(chan error, 8)}
	defer close(sp.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := New(ctx, shutdownConfig(1), oneAgendaPanel(), project, sp)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(ran)
	}()
	<-sp.started

	start := time.Now()
	cancel()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the shutdown timeout")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("Run returned after %v, before the one-second timeout", elapsed)
	}
}
