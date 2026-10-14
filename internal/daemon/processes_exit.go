package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func processFieldError(ctx context.Context, dir, field string, err error) error {
	// exit_mm can remove exe/cwd before the task becomes a zombie. Wait briefly
	// for confirmed exit; an unreadable live task must still fail closed.
	for attempt := 0; attempt < 50; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, readErr := os.ReadFile(filepath.Join(dir, "stat"))
		if os.IsNotExist(readErr) {
			return os.ErrNotExist
		}
		if readErr == nil {
			end := strings.LastIndexByte(string(b), ')')
			if end >= 0 {
				f := strings.Fields(string(b[end+1:]))
				if len(f) > 0 && (f[0] == "Z" || f[0] == "X") {
					return os.ErrNotExist
				}
			}
		}
		if !os.IsNotExist(err) || attempt == 49 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("cannot inspect process %s: %v", field, err)
}
