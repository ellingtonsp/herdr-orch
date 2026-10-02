package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/store"
)

// gateModel resolves pending decision gates: pick a gate, then an option (or type a
// free-form decision when the gate has no options).
type gateModel struct {
	c      *client.Client
	gates  []store.Gate
	sel    int
	typing bool
	input  string
	msg    string
	err    error
}

type gatesMsg struct {
	g   []store.Gate
	err error
}
type resolvedMsg struct {
	g   store.Gate
	err error
}

func runGatePopup() {
	c, err := client.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c.Caller = "human"
	if _, err := tea.NewProgram(&gateModel{c: c}).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (m *gateModel) load() tea.Cmd {
	c := m.c
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var gs []store.Gate
		err := c.Call(ctx, "gate.list", daemon.GateListArgs{}, &gs)
		return gatesMsg{gs, err}
	}
}

func (m *gateModel) resolve(id, decision string) tea.Cmd {
	c := m.c
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var g store.Gate
		err := c.Call(ctx, "gate.resolve", daemon.GateResolveArgs{ID: id, Decision: decision}, &g)
		return resolvedMsg{g, err}
	}
}

func (m *gateModel) Init() tea.Cmd { return m.load() }

func (m *gateModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case gatesMsg:
		m.gates, m.err = msg.g, msg.err
		if m.sel >= len(m.gates) {
			m.sel = 0
		}
	case resolvedMsg:
		if msg.err != nil {
			m.msg = red.Render(msg.err.Error())
		} else {
			m.msg = green.Render(fmt.Sprintf("%s resolved: %s", msg.g.ID, msg.g.Decision))
		}
		m.typing, m.input = false, ""
		return m, m.load()
	case tea.KeyMsg:
		k := msg.String()
		if m.typing {
			switch k {
			case "esc":
				m.typing, m.input = false, ""
			case "enter":
				if strings.TrimSpace(m.input) != "" && len(m.gates) > 0 {
					return m, m.resolve(m.gates[m.sel].ID, strings.TrimSpace(m.input))
				}
			case "backspace":
				if r := []rune(m.input); len(r) > 0 {
					m.input = string(r[:len(r)-1])
				}
			default:
				if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
					m.input += string(msg.Runes)
					if msg.Type == tea.KeySpace {
						m.input += " "
					}
				}
			}
			return m, nil
		}
		switch k {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.sel > 0 {
				m.sel--
			}
		case "down", "j":
			if m.sel < len(m.gates)-1 {
				m.sel++
			}
		case "enter", "t":
			if len(m.gates) > 0 {
				m.typing = true
			}
		default:
			if len(k) == 1 && k[0] >= '1' && k[0] <= '9' && len(m.gates) > 0 {
				g := m.gates[m.sel]
				i := int(k[0] - '1')
				if i < len(g.Options) {
					return m, m.resolve(g.ID, g.Options[i])
				}
			}
		}
	}
	return m, nil
}

func (m *gateModel) View() string {
	var b strings.Builder
	b.WriteString("\n " + sect.Render("Decision gates") + "\n\n")
	if m.err != nil {
		b.WriteString(" " + red.Render(m.err.Error()) + "\n")
		return b.String()
	}
	if len(m.gates) == 0 {
		b.WriteString(dim.Render("  nothing pending") + "\n\n " + dim.Render("q to close") + "\n")
		if m.msg != "" {
			b.WriteString("\n " + m.msg + "\n")
		}
		return b.String()
	}
	for i, g := range m.gates {
		cur := "  "
		if i == m.sel {
			cur = mauve.Render("› ")
		}
		fmt.Fprintf(&b, "%s%s %s task %s\n", cur, bold.Render(g.ID), statusStyle(g.Status).Render(g.Status), or(g.TaskID, "-"))
		fmt.Fprintf(&b, "     %s\n", g.Question)
		if i == m.sel {
			for j, o := range g.Options {
				fmt.Fprintf(&b, "     %s %s\n", cyan.Render(fmt.Sprintf("[%d]", j+1)), o)
			}
		}
		b.WriteString("\n")
	}
	if m.typing {
		fmt.Fprintf(&b, " decision: %s█  %s\n", m.input, dim.Render("enter to resolve · esc to cancel"))
	} else {
		b.WriteString(" " + dim.Render("↑↓ select · 1-9 pick option · enter type a decision · q close") + "\n")
	}
	if m.msg != "" {
		b.WriteString("\n " + m.msg + "\n")
	}
	return b.String()
}
