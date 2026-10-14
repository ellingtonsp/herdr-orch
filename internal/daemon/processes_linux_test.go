package daemon

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
	"os"
	"testing"
)

func TestNativeInventoryExcludesNondumpableProcess(t *testing.T) {
	if os.Getenv("HORCH_TEST_NONDUMPABLE") == "1" {
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
		time.Sleep(30 * time.Second)
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged proc ownership boundary")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeInventoryExcludesNondumpableProcess$")
	cmd.Env = append(os.Environ(), "HORCH_TEST_NONDUMPABLE=1")
	cmd.Dir = t.TempDir()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		ready <- s.Scan() && s.Text() == "ready"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper did not become nondumpable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper readiness timed out")
	}
	if _, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", cmd.Process.Pid)); !os.IsPermission(err) {
		t.Fatalf("helper did not establish protected cwd: %v", err)
	}
	ps, err := (nativeProcesses{}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundSelf := false
	for _, p := range ps {
		if p.PID == cmd.Process.Pid {
			t.Fatal("protected helper entered cleanup inventory")
		}
		if p.PID == os.Getpid() {
			foundSelf = true
		}
	}
	if !foundSelf {
		t.Fatal("ordinary same-user process missing from inventory")
	}
}
