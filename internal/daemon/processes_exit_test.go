package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessFieldErrorRecognizesZombieTransition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("42 (helper) Z 1 2 3"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := processFieldError(context.Background(), dir, "exe", os.ErrNotExist); !os.IsNotExist(err) {
		t.Fatalf("zombie reported as live: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("42 (helper) S 1 2 3"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := processFieldError(context.Background(), dir, "exe", os.ErrNotExist); os.IsNotExist(err) {
		t.Fatal("unreadable live executable hidden")
	}
}

func TestProcessFieldErrorRecognizesReapedTransition(t *testing.T) {
	// The directory check can win the race while the following stat read loses it.
	dir := t.TempDir()
	if err := processFieldError(context.Background(), dir, "exe", os.ErrNotExist); !os.IsNotExist(err) {
		t.Fatalf("reaped process reported as live: %v", err)
	}
}

func TestProcessFieldErrorWaitsForExitState(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "stat")
	if err := os.WriteFile(stat, []byte("42 (helper) R 1 2 3"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(15 * time.Millisecond)
		done <- os.WriteFile(stat, []byte("42 (helper) Z 1 2 3"), 0600)
	}()
	err := processFieldError(context.Background(), dir, "exe", os.ErrNotExist)
	if writeErr := <-done; writeErr != nil {
		t.Fatal(writeErr)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("exit transition: %v", err)
	}
}

func TestProcessFieldErrorCancellationIsNotExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := processFieldError(ctx, t.TempDir(), "exe", os.ErrNotExist); err != context.Canceled {
		t.Fatalf("cancellation reported as exit: %v", err)
	}
}
