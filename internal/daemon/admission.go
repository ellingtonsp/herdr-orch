package daemon

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// HostMetrics uses the one-minute load average and reclaimable memory percentage.
// Linux uses MemAvailable. macOS counts free, inactive and speculative VM pages.
type HostMetrics struct {
	Load           float64
	FreeMemPercent float64
}

func (e *Engine) checkAdmission(ctx context.Context, run store.Run) error {
	if run.MaxLoad == 0 && run.MinFreeMem == 0 {
		return nil
	}
	collect := e.cfg.SystemMetrics
	if collect == nil {
		collect = systemMetrics
	}
	metrics, err := collect(ctx)
	if err != nil {
		return refusal("admission_unavailable", "cannot check host capacity: %v", err)
	}
	if math.IsNaN(metrics.Load) || math.IsInf(metrics.Load, 0) || metrics.Load < 0 || math.IsNaN(metrics.FreeMemPercent) || math.IsInf(metrics.FreeMemPercent, 0) || metrics.FreeMemPercent < 0 || metrics.FreeMemPercent > 100 {
		return refusal("admission_unavailable", "host capacity metrics are invalid")
	}
	if run.MaxLoad > 0 && metrics.Load > run.MaxLoad {
		return refusal("admission_load", "dispatch refused: 1-minute load %.2f exceeds max %.2f", metrics.Load, run.MaxLoad)
	}
	if run.MinFreeMem > 0 && metrics.FreeMemPercent < run.MinFreeMem {
		return refusal("admission_memory", "dispatch refused: free/reclaimable memory %.1f%% is below minimum %.1f%%", metrics.FreeMemPercent, run.MinFreeMem)
	}
	return nil
}

func systemMetrics(ctx context.Context) (HostMetrics, error) {
	switch runtime.GOOS {
	case "linux":
		load, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return HostMetrics{}, err
		}
		mem, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return HostMetrics{}, err
		}
		return parseLinuxMetrics(string(load), string(mem))
	case "darwin":
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		load, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return HostMetrics{}, fmt.Errorf("load average: %w", err)
		}
		total, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return HostMetrics{}, fmt.Errorf("total memory: %w", err)
		}
		vm, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
		if err != nil {
			return HostMetrics{}, fmt.Errorf("VM statistics: %w", err)
		}
		return parseDarwinMetrics(string(load), string(total), string(vm))
	default:
		return HostMetrics{}, fmt.Errorf("host admission is unsupported on %s", runtime.GOOS)
	}
}
func parseLoad(s string) (float64, error) {
	fields := strings.Fields(strings.Trim(s, "{} \r\n"))
	if len(fields) == 0 {
		return 0, fmt.Errorf("missing load average")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("invalid load average")
	}
	return v, nil
}
func memoryPercent(available, total float64) (float64, error) {
	if total <= 0 || available < 0 || available > total {
		return 0, fmt.Errorf("invalid memory statistics")
	}
	return available / total * 100, nil
}
func parseLinuxMetrics(load, mem string) (HostMetrics, error) {
	m := HostMetrics{}
	var err error
	m.Load, err = parseLoad(load)
	if err != nil {
		return m, err
	}
	values := map[string]float64{}
	for _, line := range strings.Split(mem, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		key := strings.TrimSuffix(f[0], ":")
		if key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return m, fmt.Errorf("invalid %s unit", key)
		}
		v, e := strconv.ParseFloat(f[1], 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return m, fmt.Errorf("invalid %s", key)
		}
		values[key] = v
	}
	total, tok := values["MemTotal"]
	avail, aok := values["MemAvailable"]
	if !tok || !aok {
		return m, fmt.Errorf("missing MemTotal or MemAvailable")
	}
	m.FreeMemPercent, err = memoryPercent(avail, total)
	return m, err
}
func parseDarwinMetrics(load, total, vm string) (HostMetrics, error) {
	m := HostMetrics{}
	var err error
	m.Load, err = parseLoad(load)
	if err != nil {
		return m, err
	}
	bytes, err := strconv.ParseUint(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return m, fmt.Errorf("invalid total memory")
	}
	lines := strings.Split(vm, "\n")
	if len(lines) == 0 {
		return m, fmt.Errorf("missing VM statistics")
	}
	_, pageText, ok := strings.Cut(lines[0], "page size of ")
	if !ok {
		return m, fmt.Errorf("missing VM page size")
	}
	fields := strings.Fields(pageText)
	if len(fields) == 0 {
		return m, fmt.Errorf("missing VM page size")
	}
	page, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || page == 0 {
		return m, fmt.Errorf("invalid VM page size")
	}
	pages := float64(0)
	seen := map[string]bool{}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if key != "Pages free" && key != "Pages inactive" && key != "Pages speculative" {
			continue
		}
		n, err := strconv.ParseUint(strings.Trim(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			return m, fmt.Errorf("invalid %s", key)
		}
		pages += float64(n)
		seen[key] = true
	}
	if len(seen) != 3 {
		return m, fmt.Errorf("missing available VM page counts")
	}
	m.FreeMemPercent, err = memoryPercent(pages*float64(page), float64(bytes))
	return m, err
}
