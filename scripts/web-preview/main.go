// web-preview serves synthetic data for screenshots and manual UI checks. It
// owns only temporary state, and never resolves or connects to a user's session.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ellingtonsp/herdr-orch/internal/config"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/herdr"
	"github.com/ellingtonsp/herdr-orch/internal/store"
	"github.com/ellingtonsp/herdr-orch/internal/web"
)

type quietHerdr struct{ daemon.Herdr }

func (quietHerdr) PaneTokens(context.Context, string, map[string]*string) error { return nil }
func (quietHerdr) Notify(context.Context, string, string, bool) error           { return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.MkdirTemp("", "hw-preview-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "preview.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	cfg := daemon.DefaultConfig()
	cfg.UserConfig = func() (config.Config, error) {
		return config.Config{Owner: config.Owner{Principal: "you"}, DefaultProject: "acme"}, nil
	}
	engine := daemon.NewEngine(st, quietHerdr{}, cfg)
	sock, err := net.Listen("unix", filepath.Join(dir, "orch.sock"))
	if err != nil {
		return err
	}
	defer sock.Close()
	go daemon.Serve(ctx, sock, engine)
	client := web.SocketClient{Socket: sock.Addr().String()}
	md, err := os.ReadFile("internal/planmd/testdata/example.plan.md")
	if err != nil {
		return err
	}
	if err := client.Call(ctx, "plan.import", "human", daemon.PlanImportArgs{PlanRef: daemon.PlanRef{Day: "2026-03-14"}, Markdown: string(md)}, nil); err != nil {
		return err
	}
	run, err := st.CreateRun("Synthetic export work", "")
	if err != nil {
		return err
	}
	for _, row := range []struct{ pane, name, status, item, title string }{{"w4:p1", "builder", "working", "B1", "Build the CSV export"}, {"w6:p1", "reviewer", "blocked", "I1r", "Review the settings layout"}} {
		if _, err := st.RegisterWorker(store.Worker{PaneID: row.pane, Name: row.name, Agent: "codex", Worktree: "/tmp/acme"}); err != nil {
			return err
		}
		task, err := st.CreateTask(run.ID, row.title, "Synthetic preview task", nil)
		if err != nil {
			return err
		}
		dispatch, err := st.BeginDispatch(task.ID, row.pane, "codex", "")
		if err != nil {
			return err
		}
		ref := fmt.Sprintf("horch:%s/%s/%s@%s", run.ID, task.ID, dispatch.ID, row.pane)
		model := "default"
		if err := client.Call(ctx, "plan.item.update", "human", daemon.PlanItemArgs{Item: row.item, Patch: store.ItemPatch{DispatchRef: &ref, Model: &model}}, nil); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]string{"pane_id": row.pane, "agent_status": row.status})
		engine.HandleEvent(herdr.Event{Name: "pane.agent_status_changed", Data: data})
	}
	if _, err := st.InsertMessage(store.Message{From: "w6:p1", To: "human", Kind: "question", Subject: "Choose the review scope", Body: "Should the export preview include an empty-state example?"}); err != nil {
		return err
	}
	if _, err := st.InsertMessage(store.Message{From: "w4:p1", To: "human", Kind: "note", Subject: "Export preview ready", Body: "Synthetic sample rows are ready for review."}); err != nil {
		return err
	}
	if _, err := st.CreateGate(run.ID, "", "Approve the export sample?", []string{"Review", "Hold"}, 0); err != nil {
		return err
	}
	title, laneA, laneB := "Mobile layout review", "A", "B"
	for i, lane := range []*string{&laneA, &laneB} {
		if err := client.Call(ctx, "plan.item.add", "human", daemon.PlanItemArgs{Item: fmt.Sprintf("ACME-%d", 70+i), Patch: store.ItemPatch{Title: &title, Lane: lane}}, nil); err != nil {
			return err
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Printf("http://%s\n", ln.Addr())
	return web.Serve(ctx, ln, web.New(client, config.Web{OwnerPrincipal: "you", AllowLocalWrites: true}))
}
