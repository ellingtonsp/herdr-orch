package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessFieldErrorRecognizesZombieTransition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("42 (helper) Z 1 2 3"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := processFieldError(dir, "exe", os.ErrNotExist); !os.IsNotExist(err) {
		t.Fatalf("zombie reported as live: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("42 (helper) S 1 2 3"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := processFieldError(dir, "exe", os.ErrNotExist); os.IsNotExist(err) {
		t.Fatal("unreadable live executable hidden")
	}
}
