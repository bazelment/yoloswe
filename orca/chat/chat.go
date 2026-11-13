// Package chat provides the small owner-facing Bubble Tea model used by orca chat.
package chat

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/bazelment/yoloswe/orca/router"
)

type AskFunc func(context.Context, string) (string, error)

//nolint:govet // field grouping keeps the model readable.
type Model struct {
	ctx           context.Context
	ask           AskFunc
	router        router.Router
	runID         string
	input         string
	lines         []string
	status        string
	waiting       string
	width, height int
}

func New(ctx context.Context, runID string, ask AskFunc, r router.Router) Model {
	return Model{ctx: ctx, runID: runID, ask: ask, router: r, status: "goal: loading | lanes: 0 | open questions: 0 | cost today: $0.00"}
}
func (m Model) Init() tea.Cmd { return nil }

//nolint:govet // message payload is intentionally compact.
type replyMsg struct {
	text string
	err  error
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
	case tea.KeyPressMsg:
		if (x.Code == 'c' && x.Mod == tea.ModCtrl) || x.Code == tea.KeyEscape {
			return m, tea.Quit
		}
		if x.Code == tea.KeyEnter {
			text := strings.TrimSpace(m.input)
			m.input = ""
			if text == "" {
				return m, nil
			}
			if strings.HasPrefix(text, "/") {
				m.applySlash(text)
				return m, nil
			}
			m.lines = append(m.lines, "you> "+text)
			if m.waiting != "" {
				m.lines = append(m.lines, "answer> "+text)
				m.waiting = ""
				return m, nil
			}
			if m.router != nil {
				d, e := m.router.Route(m.ctx, text)
				if e != nil {
					m.lines = append(m.lines, "error: "+e.Error())
				} else if d.NeedsClarification {
					m.waiting = d.ClarifyingQuestion
					m.lines = append(m.lines, "owner question> "+m.waiting)
				} else {
					m.lines = append(m.lines, "route> "+string(d.Destination)+" — "+d.Echo)
				}
			}
			if m.ask != nil {
				return m, func() tea.Msg { t, e := m.ask(m.ctx, text); return replyMsg{t, e} }
			}
			return m, nil
		}
		if x.Code == tea.KeyBackspace {
			if m.input != "" {
				m.input = m.input[:len(m.input)-1]
			}
			return m, nil
		}
		if x.Text != "" {
			m.input += x.Text
		}
	case replyMsg:
		if x.err != nil {
			m.lines = append(m.lines, "error: "+x.err.Error())
		} else {
			m.lines = append(m.lines, "orca> "+x.text)
		}
	}
	return m, nil
}
func (m *Model) applySlash(s string) {
	p := strings.Fields(s)
	if len(p) == 0 {
		return
	}
	switch p[0] {
	case "/status":
		m.lines = append(m.lines, m.status)
	case "/lanes":
		m.lines = append(m.lines, "lanes by phase: unavailable")
	case "/asks":
		if m.waiting != "" {
			m.lines = append(m.lines, "owner question> "+m.waiting)
		} else {
			m.lines = append(m.lines, "no open questions")
		}
	case "/memory":
		m.lines = append(m.lines, "memory: attached to run "+m.runID)
	case "/plan":
		m.lines = append(m.lines, "plan: attached to run "+m.runID)
	case "/log":
		m.lines = append(m.lines, "log: "+strings.TrimSpace(strings.TrimPrefix(s, "/log")))
	case "/pause":
		m.lines = append(m.lines, "run paused")
	case "/resume":
		m.lines = append(m.lines, "run resumed")
	default:
		m.lines = append(m.lines, "commands: /status /lanes /asks /memory /plan /log <trace> /pause /resume")
	}
}
func (m Model) View() tea.View {
	var b strings.Builder
	b.WriteString("orca chat — run " + m.runID + "\n")
	for _, l := range m.lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n" + m.status + "\n> " + m.input)
	return tea.NewView(b.String())
}
func Run(ctx context.Context, in io.Reader, out io.Writer, m Model) error {
	if in != os.Stdin {
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if line == "/exit" || line == "/quit" {
				break
			}
			if strings.HasPrefix(line, "/") {
				m.applySlash(line)
				for _, v := range m.lines[len(m.lines)-1:] {
					if _, e := fmt.Fprintln(out, v); e != nil {
						return e
					}
				}
				continue
			}
			if m.ask == nil {
				continue
			}
			ans, e := m.ask(ctx, line)
			if e != nil {
				return e
			}
			if _, e = fmt.Fprintln(out, ans); e != nil {
				return e
			}
		}
		return scanner.Err()
	}
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
