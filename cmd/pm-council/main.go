// Command pm-council runs the Pepgent PM council.
//
//	pm-council app    --config-root <dir>   GitHub App webhook endpoint; wakes the daemon
//	pm-council daemon --config-root <dir>   agenda queue rebuilt from GitHub; panels; HTTP API
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oh-my-omp/pm-council/internal/api"
	"github.com/oh-my-omp/pm-council/internal/app"
	"github.com/oh-my-omp/pm-council/internal/config"
	"github.com/oh-my-omp/pm-council/internal/daemon"
	"github.com/oh-my-omp/pm-council/internal/github"
	"github.com/oh-my-omp/pm-council/internal/panel"
	"github.com/oh-my-omp/pm-council/internal/sessions"
	"github.com/oh-my-omp/pm-council/internal/tally"
)

const usage = "usage: pm-council app|daemon --config-root <dir>"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	root := fs.String("config-root", "", "configuration directory with council/ and shared/")
	_ = fs.Parse(os.Args[2:])
	if *root == "" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch sub {
	case "app":
		err = runApp(ctx, *root)
	case "daemon":
		err = runDaemon(ctx, *root)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("pm-council "+sub+" stopped", "err", err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runApp(ctx context.Context, root string) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	rt := cfg.Runtime.App
	b, err := os.ReadFile(rt.WebhookSecretFile)
	if err != nil {
		return fmt.Errorf("council/runtime.yaml: app.webhookSecretFile: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return fmt.Errorf("council/runtime.yaml: app.webhookSecretFile: %s is empty", rt.WebhookSecretFile)
	}
	a := app.New([]byte(secret), cfg.Repos.PM.Repo, rt.DaemonURL)
	slog.Info("app listening", "addr", rt.Listen, "daemon", rt.DaemonURL)
	return serve(ctx, rt.Listen, a.Handler())
}

func runDaemon(ctx context.Context, root string) error {
	startedAt := time.Now().UTC()
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		return errors.New("GH_TOKEN is empty: the daemon reads and writes GitHub with it")
	}
	gh, err := github.New(token)
	if err != nil {
		return err
	}
	project, err := gh.ResolveProject(ctx, cfg.Repos.PM.Project, cfg.Project.Fields)
	if err != nil {
		return err
	}
	reg := sessions.NewRegistry()
	runner := panel.New(cfg, gh, project, reg, tally.New(cfg, gh, project))
	d, err := daemon.New(ctx, cfg, gh, project, runner)
	if err != nil {
		return err
	}
	// The worker owns the panels; stopping it kills their sessions. Wait for that before exiting.
	ctx, cancel := context.WithCancel(ctx)
	ran := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(ran)
	}()
	defer func() {
		cancel()
		<-ran
	}()
	srv := api.New(d, reg, cfg.Goals, cfg.Repos.PM.Repo, cfg.Runtime.SessionsDir, startedAt)
	slog.Info("daemon listening", "addr", cfg.Runtime.Daemon.Listen, "project", project.ID, "issues", len(d.Queue()))
	return serve(ctx, cfg.Runtime.Daemon.Listen, srv.Handler())
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
