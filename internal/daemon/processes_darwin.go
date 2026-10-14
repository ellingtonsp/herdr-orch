package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func (nativeProcesses) List(ctx context.Context) ([]ProcessRecord, error) {
	return darwinProcesses(ctx, 0)
}
func (nativeProcesses) Inspect(ctx context.Context, pid int) (ProcessRecord, error) {
	ps, err := darwinProcesses(ctx, pid)
	if err != nil {
		return ProcessRecord{}, err
	}
	for _, p := range ps {
		if p.PID == pid {
			return p, nil
		}
	}
	return ProcessRecord{}, os.ErrNotExist
}
func darwinProcesses(ctx context.Context, pid int) ([]ProcessRecord, error) {
	args := []string{"-axo", "pid=,ppid=,uid=,lstart=,stat=,comm="}
	if pid > 0 {
		args = []string{"-p", strconv.Itoa(pid), "-o", "pid=,ppid=,uid=,lstart=,stat=,comm="}
	}
	cmd := exec.CommandContext(ctx, "/bin/ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *exec.ExitError
		if pid > 0 && len(data) == 0 && errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("process inventory: %w", err)
	}
	ps := parseProcessTable(string(data), os.Getuid())
	if len(ps) == 0 {
		return ps, nil
	}
	args = []string{"-nP", "-a", "-u", strconv.Itoa(os.Getuid()), "-d", "cwd", "-Fpn0"}
	if pid > 0 {
		args = append(args, "-p", strconv.Itoa(pid))
	}
	data, err = exec.CommandContext(ctx, "/usr/sbin/lsof", args...).Output()
	if err != nil {
		if pid > 0 && darwinProcessGone(ctx, pid) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("process cwd inventory: %w", err)
	}
	cwds := parseCwdTable(data)
	out := []ProcessRecord{}
	for _, p := range ps {
		if cwd := cwds[p.PID]; cwd != "" {
			p.Cwd = cwd
			out = append(out, p)
		} else {
			if pid == 0 {
				fresh, inspectErr := darwinProcesses(ctx, p.PID)
				if os.IsNotExist(inspectErr) {
					continue
				}
				if inspectErr != nil {
					return nil, inspectErr
				}
				out = append(out, fresh...)
				continue
			}
			// A process can exit between ps and lsof. Only a confirmed exit permits
			// omitting it; unreadable cwd is an incomplete inventory.
			check := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(p.PID), "-o", "stat=")
			state, checkErr := check.Output()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var exit *exec.ExitError
			gone := errors.As(checkErr, &exit) && exit.ExitCode() == 1 && len(state) == 0
			if gone || (checkErr == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z")) {
				continue
			}
			return nil, fmt.Errorf("cannot inspect cwd of process %d", p.PID)
		}
	}
	return out, nil
}

func darwinProcessGone(ctx context.Context, pid int) bool {
	data, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if ctx.Err() != nil {
		return false
	}
	var exit *exec.ExitError
	return (errors.As(err, &exit) && exit.ExitCode() == 1 && len(data) == 0) || (err == nil && strings.HasPrefix(strings.TrimSpace(string(data)), "Z"))
}
