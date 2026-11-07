// Package app is the council webhook entry point: it verifies webhook deliveries and wakes the
// daemon with the issue they concern. It keeps no state.
package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oh-my-omp/pm-council/internal/api"
	"github.com/oh-my-omp/pm-council/internal/queue"
)

// MaxBody is the largest webhook body accepted (GitHub caps payloads at 25 MiB).
const MaxBody = 25 << 20

// forwarded are the events that wake the daemon.
var forwarded = map[string]bool{"issues": true, "issue_comment": true, "sub_issues": true, "issue_dependencies": true}

type App struct {
	secret    []byte
	pmRepo    string
	daemonURL string
	client    *http.Client
}

func New(secret []byte, pmRepo, daemonURL string) *App {
	return &App{secret: secret, pmRepo: pmRepo, daemonURL: strings.TrimRight(daemonURL, "/"), client: &http.Client{Timeout: 10 * time.Second}}
}

// Handler serves POST /webhook.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", a.webhook)
	return mux
}

// ValidSignature reports whether header is "sha256=<hex HMAC-SHA256 of body under secret>".
func ValidSignature(secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

type payload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue        *number `json:"issue"`
	BlockedIssue *number `json:"blocked_issue"`
	ParentIssue  *number `json:"parent_issue"`
}

type number struct {
	Number int `json:"number"`
}

func (a *App) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !ValidSignature(a.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("webhook rejected: bad or missing signature", "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !forwarded[event] {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(p.Repository.FullName, a.pmRepo) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var n int
	switch {
	case event == "issue_dependencies" && p.BlockedIssue != nil:
		n = p.BlockedIssue.Number
	case event == "sub_issues" && p.ParentIssue != nil:
		n = p.ParentIssue.Number
	case p.Issue != nil:
		n = p.Issue.Number
	}
	if n <= 0 {
		slog.Warn("webhook without an issue number", "event", event, "action", p.Action)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ev := api.Event{Issue: string(queue.FormatRef(p.Repository.FullName, n)), Event: event, Action: p.Action}
	if err := a.forward(r.Context(), ev); err != nil {
		slog.Error("forward to daemon failed", "issue", ev.Issue, "event", event, "err", err)
		http.Error(w, "forward to daemon failed", http.StatusBadGateway)
		return
	}
	slog.Info("forwarded", "issue", ev.Issue, "event", event, "action", p.Action)
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) forward(ctx context.Context, ev api.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.daemonURL+"/v1/events", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon answered %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
