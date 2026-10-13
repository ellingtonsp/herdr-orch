package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestMailboxCLI(t *testing.T) {
	startDaemon(t)
	mustHorch(t, "w1:p1", "run", "create", "--title", "Generic mailbox fixture")
	mustHorch(t, "w1:p1", "send", "--to", "human", "--body", "Generic fixture message")
	raw := mustHorch(t, "human", "check", "--peek", "--json")
	var msgs []store.Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil || len(msgs) != 1 || msgs[0].ReadAt != 0 {
		t.Fatalf("peek: %s %v", raw, err)
	}
	if out := mustHorch(t, "human", "ack", "--id", msgs[0].ID); !strings.Contains(out, "acknowledged "+msgs[0].ID) {
		t.Fatalf("ack: %s", out)
	}
	if out := mustHorch(t, "human", "check", "--peek", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("ack left message unread: %s", out)
	}
	if _, err := horch(t, "human", "ack"); err == nil {
		t.Fatal("ack without id accepted")
	}
	if out := mustHorch(t, "w1:p1", "inbox"); !strings.Contains(out, "delivery=picked_up (read)") {
		t.Fatalf("receipt not shown: %s", out)
	}
	if _, err := horch(t, "w1:p1", "send", "--to", "human", "--body", "next", "--wait-pickup", "--timeout", "1ms"); err == nil {
		t.Fatal("unread human message reported pickup")
	}
	if got := consumerLabel(); !strings.HasSuffix(got, fmt.Sprintf(":%d", os.Getpid())) {
		t.Fatalf("consumer label: %s", got)
	}
}
