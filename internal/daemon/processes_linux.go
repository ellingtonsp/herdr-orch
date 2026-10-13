package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func (h nativeProcesses) List(ctx context.Context) ([]ProcessRecord, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := []ProcessRecord{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		st, err := os.Stat(filepath.Join("/proc", entry.Name()))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			continue
		}
		p, err := h.Inspect(ctx, pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
func (nativeProcesses) Inspect(ctx context.Context, pid int) (ProcessRecord, error) {
	if err := ctx.Err(); err != nil {
		return ProcessRecord{}, err
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return ProcessRecord{}, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return ProcessRecord{}, fmt.Errorf("invalid process stat for %d", pid)
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 {
		return ProcessRecord{}, fmt.Errorf("short process stat for %d", pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return ProcessRecord{}, os.ErrNotExist
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return ProcessRecord{}, err
	}
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return ProcessRecord{}, processFieldError(dir, "cwd", err)
	}
	command, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		return ProcessRecord{}, processFieldError(dir, "exe", err)
	}
	return ProcessRecord{PID: pid, Parent: parent, Started: fields[19], Command: strings.TrimSuffix(command, " (deleted)"), Cwd: strings.TrimSuffix(cwd, " (deleted)")}, nil
}

func processFieldError(dir, field string, err error) error {
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		return os.ErrNotExist
	}
	return fmt.Errorf("cannot inspect process %s: %v", field, err)
}
