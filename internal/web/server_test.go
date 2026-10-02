package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

type quietHerdr struct{ daemon.Herdr }

func (quietHerdr) PaneTokens(context.Context, string, map[string]*string) error { return nil }
func (quietHerdr) Notify(context.Context, string, string, bool) error           { return nil }

type harness struct {
	server *Server
	http   *httptest.Server
	client SocketClient
	store  *store.Store
	engine *daemon.Engine
}

func setup(t *testing.T, local bool) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "hw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := daemon.DefaultConfig()
	cfg.UserConfig = func() (config.Config, error) {
		return config.Config{Owner: config.Owner{Principal: "you"}, DefaultProject: "acme"}, nil
	}
	e := daemon.NewEngine(st, quietHerdr{}, cfg)
	ln, err := net.Listen("unix", filepath.Join(dir, "orch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go daemon.Serve(ctx, ln, e)
	t.Cleanup(func() { cancel(); ln.Close(); st.Close() })
	c := SocketClient{Socket: ln.Addr().String()}
	md, err := os.ReadFile("../planmd/testdata/example.plan.md")
	if err != nil {
		t.Fatal(err)
	}
	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	defer ccancel()
	if err := c.Call(cctx, "plan.import", "human", daemon.PlanImportArgs{PlanRef: daemon.PlanRef{Day: "2026-03-14"}, Markdown: string(md)}, nil); err != nil {
		t.Fatal(err)
	}
	s := New(c, config.Web{OwnerPrincipal: "you", AllowLocalWrites: local})
	s.poll = 20 * time.Millisecond
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return &harness{s, hs, c, st, e}
}
func request(t *testing.T, h *harness, path, principal string, args any) (int, []byte) {
	t.Helper()
	method := "GET"
	var body io.Reader
	if args != nil {
		method = "POST"
		data, _ := json.Marshal(args)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, h.http.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if args != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if principal != "" {
		req.Header.Set("Tailscale-User-Login", principal)
	}
	res, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}
func current(t *testing.T, h *harness) store.PlanView {
	t.Helper()
	status, b := request(t, h, "/api/plan", "", nil)
	var out daemon.PlanExport
	if status != 200 || json.Unmarshal(b, &out) != nil || out.View == nil {
		t.Fatalf("plan: %d %s", status, b)
	}
	return *out.View
}
func argsFor(t *testing.T, v store.PlanView, id string) daemon.PlanItemArgs {
	t.Helper()
	it, ok := v.Item(id)
	if !ok {
		t.Fatal("missing item", id)
	}
	return daemon.PlanItemArgs{PlanRef: daemon.PlanRef{Project: v.Plan.Project, Day: v.Plan.Day, Principal: "guest"}, Item: id, IfVersion: it.Version, IfPlanVersion: v.Plan.Version}
}
func TestReadAPIAndAssets(t *testing.T) {
	h := setup(t, false)
	for _, path := range []string{"/", "/app.js", "/style.css", "/api/plan", "/api/activity", "/api/identity"} {
		status, b := request(t, h, path, "", nil)
		if status != 200 || len(b) == 0 {
			t.Fatalf("%s: %d %s", path, status, b)
		}
	}
	v := current(t, h)
	if v.Plan.Project != "acme" || len(v.Items) != 12 || len(v.Decisions) != 3 {
		t.Fatalf("fixture: %+v", v)
	}
	status, b := request(t, h, "/api/plan?project=missing", "", nil)
	if status != 404 || !bytes.Contains(b, []byte("no_plan")) {
		t.Fatalf("missing plan: %d %s", status, b)
	}
	res, err := h.http.Client().Get(h.http.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("missing security headers")
	}
}
func TestOwnerWritesAndVersions(t *testing.T) {
	h := setup(t, false)
	first := current(t, h)
	hold := argsFor(t, first, "B1")
	hold.Reason = "Waiting for review"
	for _, identity := range []string{"", "guest"} {
		status, b := request(t, h, "/api/plan/items/hold", identity, hold)
		if status != 403 {
			t.Fatalf("identity %q: %d %s", identity, status, b)
		}
	}
	status, b := request(t, h, "/api/plan/items/hold", "you", hold)
	var write store.PlanWrite
	if status != 200 || json.Unmarshal(b, &write) != nil || !write.Events[0].Approval || write.Events[0].Principal != "you" || write.Events[0].Actor != "web:you" {
		t.Fatalf("hold: %d %s", status, b)
	}
	// A stale view of a different item must fail too (whole-plan fencing).
	stale := argsFor(t, first, "R1")
	stale.Position = 2
	status, b = request(t, h, "/api/plan/items/move", "you", stale)
	if status != 409 || !bytes.Contains(b, []byte("version_conflict")) {
		t.Fatalf("conflict: %d %s", status, b)
	}
	fresh := current(t, h)
	item, _ := fresh.Item("B1")
	if !item.Held {
		t.Fatal("hold not stored")
	}
	release := argsFor(t, fresh, "B1")
	status, b = request(t, h, "/api/plan/items/release", "you", release)
	if status != 200 {
		t.Fatalf("release: %d %s", status, b)
	}
	moved := argsFor(t, current(t, h), "B1")
	moved.Position = 1
	status, b = request(t, h, "/api/plan/items/move", "you", moved)
	if status != 200 {
		t.Fatalf("move: %d %s", status, b)
	}
	v := current(t, h)
	if v.Items[0].ID != "B1" {
		t.Fatal("move order incorrect")
	}
	title, lane := "Export preview", "A"
	add := daemon.PlanItemArgs{PlanRef: daemon.PlanRef{Project: "acme", Day: v.Plan.Day}, Item: "ACME-12", IfPlanVersion: v.Plan.Version, Patch: store.ItemPatch{Title: &title, Lane: &lane}}
	status, b = request(t, h, "/api/plan/items/add", "you", add)
	if status != 200 {
		t.Fatalf("add: %d %s", status, b)
	}
	status, _ = request(t, h, "/api/plan/items/add", "you", add)
	if status != 409 {
		t.Fatalf("stale add: %d", status)
	}
	remove := argsFor(t, current(t, h), "ACME-12")
	status, b = request(t, h, "/api/plan/items/remove", "you", remove)
	if status != 200 {
		t.Fatalf("remove: %d %s", status, b)
	}
	if _, ok := current(t, h).Item("ACME-12"); ok {
		t.Fatal("remove not stored")
	}
	// Individual item versions cannot be omitted or stale, even on a fresh plan.
	invalid := argsFor(t, current(t, h), "B1")
	invalid.IfVersion = hold.IfVersion
	status, _ = request(t, h, "/api/plan/items/hold", "you", invalid)
	if status != 409 {
		t.Fatal("stale item accepted")
	}
	invalid.IfVersion = 0
	status, _ = request(t, h, "/api/plan/items/hold", "you", invalid)
	if status != 400 {
		t.Fatal("missing item version accepted")
	}
	invalid.IfVersion = 1
	invalid.IfPlanVersion = 0
	status, _ = request(t, h, "/api/plan/items/hold", "you", invalid)
	if status != 400 {
		t.Fatal("missing plan version accepted")
	}
}
func TestLocalWritesAndProxyIdentity(t *testing.T) {
	h := setup(t, true)
	a := argsFor(t, current(t, h), "B1")
	a.Reason = "Review pending"
	status, b := request(t, h, "/api/plan/items/hold", "", a)
	if status != 200 {
		t.Fatalf("local opt-in: %d %s", status, b)
	}
	// A non-owner header never falls back to the local exception.
	a = argsFor(t, current(t, h), "R1")
	status, _ = request(t, h, "/api/plan/items/hold", "guest", a)
	if status != 403 {
		t.Fatal("guest header accepted through local exception")
	}
	for _, peer := range []string{"192.0.2.1:9000", "[::1]:9000", "127.0.0.1:9000"} {
		req := httptest.NewRequest("GET", "/api/identity", nil)
		req.RemoteAddr = peer
		_, allowed := h.server.identity(req)
		if allowed != (peer != "192.0.2.1:9000") {
			t.Fatalf("peer %s: %v", peer, allowed)
		}
	}
	req := httptest.NewRequest("GET", "/api/identity", nil)
	req.Header.Add("Tailscale-User-Login", "you")
	req.Header.Add("Tailscale-User-Login", "guest")
	if _, allowed := h.server.identity(req); allowed {
		t.Fatal("duplicate identity accepted")
	}
	network := httptest.NewRequest("GET", "/api/identity", nil)
	network.RemoteAddr = "192.0.2.1:9000"
	network.Header.Set("Tailscale-User-Login", "you")
	if _, allowed := h.server.identity(network); allowed {
		t.Fatal("network peer spoofed proxy identity")
	}
	h.server.cfg.OwnerPrincipal = ""
	if _, allowed := h.server.identity(httptest.NewRequest("GET", "/api/identity", nil)); allowed {
		t.Fatal("unconfigured owner accepted")
	}
}
func TestWriteInputAndCrossOrigin(t *testing.T) {
	h := setup(t, true)
	a := argsFor(t, current(t, h), "B1")
	data, _ := json.Marshal(a)
	for _, test := range []struct {
		body, content, origin, site, path string
		status                            int
	}{
		{string(data), "text/plain", "", "", "hold", 415},
		{string(data), "application/json", "https://other.example", "", "hold", 403},
		{string(data), "application/json", "", "cross-site", "hold", 403},
		{`{"extra":true}`, "application/json", "", "", "hold", 400},
		{string(data) + ` {}`, "application/json", "", "", "hold", 400},
		{strings.Repeat("x", 40<<10), "application/json", "", "", "hold", 400},
		{string(data), "application/json", "", "", "update", 404},
	} {
		req, _ := http.NewRequest("POST", h.http.URL+"/api/plan/items/"+test.path, strings.NewReader(test.body))
		req.Header.Set("Content-Type", test.content)
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Sec-Fetch-Site", test.site)
		res, err := h.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != test.status {
			t.Fatalf("input %s: %d", test.path, res.StatusCode)
		}
	}
}
func TestAttentionAndWorkerSnapshot(t *testing.T) {
	h := setup(t, false)
	question, err := h.store.InsertMessage(store.Message{From: "w1:p2", To: "human", Kind: "question", Subject: "Choose export format"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Unread([]string{"human"}, true); err != nil {
		t.Fatal(err)
	}
	// Push the already-read question out of the recent inbox window.
	for range 55 {
		if _, err := h.store.InsertMessage(store.Message{From: "w1:p2", To: "human", Kind: "note", Body: "Synthetic progress"}); err != nil {
			t.Fatal(err)
		}
	}
	escalation, err := h.store.InsertMessage(store.Message{From: "w1:p2", To: "human", Kind: "escalation", Body: "Review required"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.RegisterWorker(store.Worker{PaneID: "w1:p2", Name: "builder", Agent: "codex", Worktree: "/tmp/acme"}); err != nil {
		t.Fatal(err)
	}
	h.engine.HandleEvent(herdr.Event{Name: "pane.agent_status_changed", Data: json.RawMessage(`{"pane_id":"w1:p2","agent_status":"blocked"}`)})
	read := func() daemon.WebActivity {
		status, b := request(t, h, "/api/activity", "", nil)
		var out daemon.WebActivity
		if status != 200 || json.Unmarshal(b, &out) != nil {
			t.Fatalf("activity: %d %s", status, b)
		}
		return out
	}
	out := read()
	if len(out.Needs) != 2 || len(out.Messages) != 50 || len(out.Workers) != 1 || out.Workers[0].LiveStatus != "blocked" {
		t.Fatalf("snapshot: %+v", out)
	}
	// Reading the web snapshot must not acknowledge the escalation.
	out = read()
	if len(out.Needs) != 2 {
		t.Fatal("read consumed attention")
	}
	if _, err := h.store.InsertMessage(store.Message{From: "human", To: "w1:p2", Kind: "reply", ReplyTo: question.ID, Body: "CSV"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Unread([]string{"human"}, true); err != nil {
		t.Fatal(err)
	}
	out = read()
	if len(out.Needs) != 0 {
		t.Fatalf("resolved attention remains: %s %+v", escalation.ID, out.Needs)
	}
}
func nextEvent(t *testing.T, scan *bufio.Scanner, kind string) (string, json.RawMessage) {
	t.Helper()
	var event, id string
	for scan.Scan() {
		line := scan.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: ") && event == kind:
			return id, json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
	t.Fatalf("missing SSE %s: %v", kind, scan.Err())
	return "", nil
}
func TestSSEDeliveryReplayAndPoll(t *testing.T) {
	h := setup(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.http.URL+"/api/events", nil)
	res, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("wrong SSE content type")
	}
	scan := bufio.NewScanner(res.Body)
	nextEvent(t, scan, "ready")
	id, _ := nextEvent(t, scan, "plan") // imported fixture, replayed
	a := argsFor(t, current(t, h), "B1")
	a.Reason = "Synthetic wait"
	status, b := request(t, h, "/api/plan/items/hold", "you", a)
	if status != 200 {
		t.Fatalf("hold: %s", b)
	}
	newID, raw := nextEvent(t, scan, "plan")
	var ev store.PlanEvent
	if json.Unmarshal(raw, &ev) != nil || ev.Op != "item.hold" || newID == id {
		t.Fatalf("SSE event: %s", raw)
	}
	_, raw = nextEvent(t, scan, "activity")
	if !bytes.Contains(raw, []byte(`"messages"`)) {
		t.Fatalf("poll: %s", raw)
	}
	res.Body.Close()
	release := argsFor(t, current(t, h), "B1")
	status, b = request(t, h, "/api/plan/items/release", "you", release)
	if status != 200 {
		t.Fatalf("release: %s", b)
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", h.http.URL+"/api/events", nil)
	req.Header.Set("Last-Event-ID", newID)
	replay, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Body.Close()
	scan = bufio.NewScanner(replay.Body)
	nextEvent(t, scan, "ready")
	_, raw = nextEvent(t, scan, "plan")
	if json.Unmarshal(raw, &ev) != nil || ev.Op != "item.release" {
		t.Fatalf("replay: %s", raw)
	}
	status, _ = request(t, h, "/api/events?since=bad", "", nil)
	if status != 400 {
		t.Fatal("invalid cursor accepted")
	}
}
func TestUnavailableDaemon(t *testing.T) {
	s := New(SocketClient{Socket: filepath.Join(t.TempDir(), "missing.sock")}, config.Web{})
	for _, path := range []string{"/api/plan", "/api/activity"} {
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, httptest.NewRequest("GET", path, nil))
		if out.Code != 503 {
			t.Fatalf("unavailable: %d", out.Code)
		}
	}
}
