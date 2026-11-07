package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFailureOfShutdownNamesTheStopAndItsCancelTime(t *testing.T) {
	now := time.Date(2026, 11, 7, 9, 0, 9, 0, time.UTC)
	cancelAt := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	aborted := errors.New("Request was aborted")

	reason, at := failureOf(aborted, Shutdown(cancelAt), now)
	if reason != shutdownReason || !at.Equal(cancelAt) {
		t.Errorf("shutdown: failureOf = %q %v, want %q %v", reason, at, shutdownReason, cancelAt)
	}

	reason, at = failureOf(aborted, nil, now)
	if reason != aborted.Error() || !at.Equal(now) {
		t.Errorf("no cause: failureOf = %q %v, want %q %v", reason, at, aborted.Error(), now)
	}
}

// A shutdown record is read back as the session's failure with the stop's cancel time, so the
// listing ends the session there, not at the last entry of its file.
func TestShutdownRecordIsReadBack(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s-shutdown.jsonl")
	header := `{"type":"session","version":3,"id":"s-shutdown","timestamp":"2026-11-07T08:00:00.000Z","cwd":"/x"}` + "\n"
	if err := os.WriteFile(file, []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	cancelAt := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	if err := RecordFailure(file, shutdownReason, cancelAt); err != nil {
		t.Fatal(err)
	}
	f, err := ParseFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if f.Failure != shutdownReason || !f.FailedAt.Equal(cancelAt) {
		t.Errorf("read back = %q %v, want %q %v", f.Failure, f.FailedAt, shutdownReason, cancelAt)
	}
}
