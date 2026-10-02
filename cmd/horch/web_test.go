package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

func TestWebCommandEndToEnd(t *testing.T) {
	// Resolve the command's real Go client into an isolated temporary session.
	dir := t.TempDir()
	for _, key := range []string{"HERDR_PLUGIN_STATE_DIR", "HORCH_AS", "HERDR_PANE_ID"} {
		t.Setenv(key, "")
	}
	t.Setenv("HORCH_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(dir, "sessions", "web-test", "herdr.sock"))
	t.Setenv("HORCH_DAEMON_BIN", "/nonexistent")
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte("[owner]\nprincipal = \"you\"\n[web]\nallow_local_writes = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HORCH_CONFIG", file)
	c, err := client.New()
	if err != nil {
		t.Fatal(err)
	}
	oldClient := cli
	cli = c
	t.Cleanup(func() { cli = oldClient })
	st, err := store.Open(c.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	cfg := daemon.DefaultConfig()
	cfg.UserConfig = func() (config.Config, error) {
		return config.Config{Owner: config.Owner{Principal: "you"}, DefaultProject: "acme"}, nil
	}
	e := daemon.NewEngine(st, nopHerdr{}, cfg)
	ln, err := net.Listen("unix", c.Paths.Sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go daemon.Serve(ctx, ln, e)
	t.Cleanup(func() { cancel(); ln.Close(); st.Close() })
	md, err := os.ReadFile("../../internal/planmd/testdata/example.plan.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "plan.import", daemon.PlanImportArgs{PlanRef: daemon.PlanRef{Day: "2026-03-14"}, Markdown: string(md)}, nil); err != nil {
		t.Fatal(err)
	}
	oldListen := webListen
	t.Cleanup(func() { webListen = oldListen })
	listening := make(chan net.Listener, 1)
	webListen = func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, address)
		if err == nil {
			listening <- ln
		}
		return ln, err
	}
	wctx, wcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- dispatch(wctx, []string{"web", "--listen", "127.0.0.1:0"}) }()
	t.Cleanup(wcancel)
	var httpLn net.Listener
	select {
	case httpLn = <-listening:
	case err := <-done:
		t.Fatalf("command: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener timeout")
	}
	base := "http://" + httpLn.Addr().String()
	hc := &http.Client{Timeout: 5 * time.Second}
	res, err := hc.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(html), "Needs you") {
		t.Fatalf("UI: %d", res.StatusCode)
	}
	res, err = hc.Get(base + "/api/plan")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot daemon.PlanExport
	if err := json.NewDecoder(res.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if snapshot.View == nil || len(snapshot.View.Items) != 12 {
		t.Fatal("synthetic plan missing")
	}
	item, _ := snapshot.View.Item("B1")
	args := daemon.PlanItemArgs{PlanRef: daemon.PlanRef{Project: "acme", Day: "2026-03-14"}, Item: "B1", IfVersion: item.Version, IfPlanVersion: snapshot.Plan.Version, Reason: "Review pending"}
	body, _ := json.Marshal(args)
	res, err = hc.Post(base+"/api/plan/items/hold", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var written store.PlanWrite
	if err := json.NewDecoder(res.Body).Decode(&written); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || len(written.Events) != 1 || written.Events[0].Principal != "you" || !written.Events[0].Approval {
		t.Fatalf("command edit: %+v", written)
	}
	// Stop HTTP while a streaming browser is connected; the daemon must survive.
	stream, err := hc.Get(base + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	wcancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web command did not stop")
	}
	check, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCheck()
	var after store.PlanView
	if err := c.Call(check, "plan.show", daemon.PlanRef{}, &after); err != nil {
		t.Fatal("web shutdown affected daemon:", err)
	}
	held, _ := after.Item("B1")
	if !held.Held {
		t.Fatal("edit not persisted")
	}
}
