package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedInput struct {
	Method string
	Params map[string]any
	At     time.Time
}

func inputServer(t *testing.T, status string, interactive bool) (*Client, func() []recordedInput) {
	return inputServerReadiness(t, status, interactive, interactive)
}
func inputServerReadiness(t *testing.T, status string, interactive, agentReady bool) (*Client, func() []recordedInput) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []recordedInput
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadBytes('\n')
				var req struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal(line, &req) != nil {
					return
				}
				mu.Lock()
				calls = append(calls, recordedInput{req.Method, req.Params, time.Now()})
				mu.Unlock()
				result := any(map[string]any{})
				if req.Method == "pane.get" || req.Method == "agent.get" {
					key := "pane"
					ready := interactive
					if req.Method == "agent.get" {
						key = "agent"
						ready = agentReady
					}
					result = map[string]any{key: map[string]any{"pane_id": "w1:p2", "workspace_id": "w1", "agent": "codex", "agent_status": status, "interactive_ready": ready}}
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"result": result})
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return New(sock), func() []recordedInput { mu.Lock(); defer mu.Unlock(); return append([]recordedInput(nil), calls...) }
}
func TestAgentPromptPreservesLongMultilinePaste(t *testing.T) {
	c, calls := inputServer(t, "idle", true)
	body := strings.Repeat("line with  two spaces\n", 500)
	if err := c.AgentPrompt(context.Background(), "w1:p2", body); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || got[0].Method != "agent.prompt" || got[0].Params["text"] != body {
		t.Fatalf("prompt not delegated intact: %+v", got)
	}
}
func TestFallbackSeparatesPasteAndEnter(t *testing.T) {
	c, calls := inputServer(t, "idle", true)
	if err := c.SendText(context.Background(), "w1:p2", "hello"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0].Method != "pane.send_text" || got[1].Method != "pane.send_keys" || got[1].At.Sub(got[0].At) < 300*time.Millisecond {
		t.Fatalf("paste boundary missing: %+v", got)
	}
}
func TestPendingPasteRetryOnlySendsEnter(t *testing.T) {
	c, calls := inputServer(t, "idle", true)
	if err := c.SubmitPending(context.Background(), "w1:p2"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 || got[2].Method != "pane.send_keys" {
		t.Fatalf("retry: %+v", got)
	}
	keys, _ := got[2].Params["keys"].([]any)
	if len(keys) != 1 || keys[0] != "enter" {
		t.Fatalf("keys: %+v", keys)
	}
}
func TestPendingPasteNeverSubmitsBusyOrStartupPane(t *testing.T) {
	for _, status := range []string{"working", "blocked", "unknown", "idle"} {
		t.Run(status, func(t *testing.T) {
			c, calls := inputServer(t, status, false)
			if err := c.SubmitPending(context.Background(), "w1:p2"); err == nil {
				t.Fatal("unsafe Enter accepted")
			}
			for _, call := range calls() {
				if call.Method == "pane.send_keys" {
					t.Fatal("sent Enter")
				}
			}
		})
	}
}

func TestCallCancellationInterruptsWaitingResponse(t *testing.T) {
	dir, err := os.MkdirTemp("", "hc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		close(received)
		var b [1]byte
		_, _ = conn.Read(b[:])
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(sock).Call(ctx, "pane.get", nil, nil) }()
	<-received
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded unexpectedly")
		}
	case <-time.After(time.Second):
		t.Fatal("RPC ignored cancellation")
	}
	<-serverDone
}

func TestPendingPasteHonorsPaneNotReadyEvenWhenAgentReady(t *testing.T) {
	c, calls := inputServerReadiness(t, "idle", false, true)
	if err := c.SubmitPending(context.Background(), "w1:p2"); err == nil {
		t.Fatal("ignored pane readiness")
	}
	for _, call := range calls() {
		if call.Method == "pane.send_keys" {
			t.Fatal("sent Enter")
		}
	}
}
