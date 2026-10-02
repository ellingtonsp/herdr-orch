package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

var (
	dim    = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	sect   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("44"))
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	cyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("44"))
	mauve  = lipgloss.NewStyle().Foreground(lipgloss.Color("170"))
)

func statusStyle(s string) lipgloss.Style {
	switch s {
	case store.TaskCompleted, "idle", "done", store.WorkerReleased:
		return green
	case store.TaskFailed, store.DispatchCircuitBroken, store.WorkerReleaseFailed, store.WorkerExited:
		return red
	case store.TaskDispatched, "working", store.TaskReady:
		return cyan
	case store.TaskBlocked, store.GatePending, store.GateTimeout:
		return yellow
	}
	return dim
}

type boardModel struct {
	c      *client.Client
	board  daemon.Board
	runIdx int
	err    error
	flash  string
	width  int
	height int
}

type boardMsg struct {
	b   daemon.Board
	err error
}
type tickMsg struct{}
type flashMsg string

func runBoard() {
	c, err := client.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c.Caller = "human"
	if _, err := tea.NewProgram(&boardModel{c: c}, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (m *boardModel) selectedRun() string {
	if len(m.board.Runs) == 0 {
		return ""
	}
	if m.runIdx >= len(m.board.Runs) {
		m.runIdx = 0
	}
	return m.board.Runs[m.runIdx].ID
}

func (m *boardModel) fetch() tea.Cmd {
	run := m.selectedRun()
	c := m.c
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var b daemon.Board
		err := c.Call(ctx, "board", daemon.RunRef{Run: run}, &b)
		return boardMsg{b, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *boardModel) Init() tea.Cmd { return tea.Batch(m.fetch(), tick()) }

func (m *boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case boardMsg:
		m.err = msg.err
		if msg.err == nil {
			m.board = msg.b
		}
	case tickMsg:
		return m, tea.Batch(m.fetch(), tick())
	case flashMsg:
		m.flash = string(msg)
		return m, m.fetch()
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "tab", "right", "l":
			if n := len(m.board.Runs); n > 0 {
				m.runIdx = (m.runIdx + 1) % n
			}
			return m, m.fetch()
		case "shift+tab", "left", "h":
			if n := len(m.board.Runs); n > 0 {
				m.runIdx = (m.runIdx + n - 1) % n
			}
			return m, m.fetch()
		case "d":
			run, c := m.selectedRun(), m.c
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				var ds []store.Dispatch
				if err := c.Call(ctx, "dispatch.next", daemon.RunRef{Run: run}, &ds); err != nil {
					return flashMsg("dispatch next: " + err.Error())
				}
				return flashMsg(fmt.Sprintf("dispatched %d task(s)", len(ds)))
			}
		case "g":
			return m, func() tea.Msg {
				if err := openPane("gate", "popup"); err != nil {
					return flashMsg(err.Error())
				}
				return flashMsg("")
			}
		case "r":
			return m, m.fetch()
		}
	}
	return m, nil
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if n > 1 && len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (m *boardModel) View() string {
	var b strings.Builder
	w := m.width
	if w <= 0 {
		w = 100
	}
	b.WriteString(sect.Render(" horch board") + dim.Render("  tab: run · d: dispatch next · g: gates · r: refresh · q: quit") + "\n")
	if m.err != nil {
		b.WriteString("\n " + red.Render("daemon: "+m.err.Error()) + "\n")
		return b.String()
	}
	if m.flash != "" {
		b.WriteString(" " + mauve.Render(m.flash) + "\n")
	}
	// Runs strip.
	b.WriteString("\n")
	if len(m.board.Runs) == 0 {
		b.WriteString(dim.Render("  no runs yet — horch run create --title ...") + "\n")
	}
	var runs []string
	for i, r := range m.board.Runs {
		label := fmt.Sprintf("%s %s", r.ID, trunc(r.Title, 24))
		if m.board.Selected != nil && r.ID == m.board.Selected.Run.ID {
			label = bold.Render("▸ " + label)
		} else if i < 8 {
			label = dim.Render("  " + label)
		} else {
			continue
		}
		runs = append(runs, label+" "+statusStyle(r.Status).Render(r.Status))
	}
	b.WriteString(" " + strings.Join(runs, "   ") + "\n")

	if s := m.board.Selected; s != nil {
		b.WriteString("\n" + sect.Render(" Tasks") + "\n")
		if len(s.Tasks) == 0 {
			b.WriteString(dim.Render("  none") + "\n")
		}
		for _, t := range s.Tasks {
			assignee := t.AssigneePaneID
			if st := m.board.Status[assignee]; assignee != "" && st != "" && t.Status == store.TaskDispatched {
				assignee += " " + statusStyle(st).Render(st)
			}
			deps := ""
			if len(t.Deps) > 0 {
				deps = dim.Render(" ← " + strings.Join(t.Deps, ","))
			}
			fmt.Fprintf(&b, "  %-4s %s %s%s %s %s\n", t.ID,
				statusStyle(t.Status).Render(fmt.Sprintf("%-10s", t.Status)),
				trunc(t.Title, w/3), deps,
				dim.Render(fmt.Sprintf("tries %d", t.Attempts)), assignee)
		}
		if len(s.Workers) > 0 {
			b.WriteString("\n" + sect.Render(" Workers") + "\n")
			for _, x := range s.Workers {
				live := m.board.Status[x.PaneID]
				if x.State != store.WorkerLive {
					live = x.State
				}
				fmt.Fprintf(&b, "  %-8s %-18s %-7s %s %s\n", x.PaneID, trunc(x.Name, 18), x.Agent, statusStyle(live).Render(or(live, "?")), dim.Render(trunc(x.Worktree, w/3)))
			}
		}
	}
	if len(m.board.Pending) > 0 {
		b.WriteString("\n" + sect.Render(" Gates") + dim.Render("  (g to resolve)") + "\n")
		for _, g := range m.board.Pending {
			opts := ""
			if len(g.Options) > 0 {
				opts = dim.Render(" [" + strings.Join(g.Options, "/") + "]")
			}
			fmt.Fprintf(&b, "  %-4s %s task %s: %s%s\n", g.ID, statusStyle(g.Status).Render(g.Status), or(g.TaskID, "-"), trunc(g.Question, w/2), opts)
		}
	}
	b.WriteString("\n" + sect.Render(" Inbox") + "\n")
	msgs := m.board.Messages
	max := 10
	if m.height > 0 {
		max = m.height - strings.Count(b.String(), "\n") - 2
	}
	if max < 3 {
		max = 3
	}
	if len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
	}
	if len(msgs) == 0 {
		b.WriteString(dim.Render("  empty") + "\n")
	}
	for _, x := range msgs {
		kind := x.Kind
		style := dim
		switch kind {
		case store.KindEscalation:
			style = red
		case store.KindQuestion:
			style = yellow
		case store.KindDone:
			style = green
		}
		unread := " "
		if x.ReadAt == 0 {
			unread = mauve.Render("•")
		}
		text := x.Subject
		if text == "" {
			text = x.Body
		}
		fmt.Fprintf(&b, " %s%-4s %s %s→%s %s\n", unread, x.ID, style.Render(fmt.Sprintf("%-10s", kind)), dim.Render(x.From), dim.Render(x.To), trunc(text, w-40))
	}
	return b.String()
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
