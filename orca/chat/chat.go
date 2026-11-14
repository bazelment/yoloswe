// Package chat provides the owner-facing Bubble Tea conversation.
package chat

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/bazelment/yoloswe/orca/router"
)

type AskFunc func(context.Context, string) (string, error)
type DispatchFunc func(context.Context, router.Decision, string, func(string)) error
type Snapshot struct {
	Status    string
	Questions []string
}
type Backend interface {
	Snapshot(context.Context) (Snapshot, error)
	Command(context.Context, string) (string, error)
}

//nolint:govet
type Model struct {
	poll                 bool
	questions            []string
	ctx                  context.Context
	ask                  AskFunc
	router               router.Router
	Dispatch             DispatchFunc
	Backend              Backend
	runID, input, status string
	lines                []string
	busy                 bool
	width, height        int
	streamLine           int
}

func New(ctx context.Context, runID string, ask AskFunc, r router.Router) Model {
	return Model{ctx: ctx, runID: runID, ask: ask, router: r}
}

// IsNewGoal reports whether text talks about starting work, so chat routes it
// to the concierge. Talking about it is not asking for it: run.start acts only
// on IsStartRequest.
func IsNewGoal(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "start a run") || strings.Contains(lower, "start work") || strings.HasPrefix(lower, "new goal")
}

// startPhrases open an explicit request to start a run.
var startPhrases = []string{
	"start a run", "start a new run", "start the run", "start run", "start work", "start working",
	"kick off a run", "launch a run", "begin a run", "new goal", "new run",
}

// politeLeads may precede a start phrase without changing its meaning.
var politeLeads = []string{"please", "ok", "okay", "alright", "now", "go ahead and", "let's", "lets", "kindly", "orca"}

// doubtWords make a request negated, conditional or tentative.
var doubtWords = map[string]bool{
	"not": true, "no": true, "never": true, "don't": true, "dont": true, "doesn't": true, "won't": true,
	"can't": true, "cannot": true, "shouldn't": true, "without": true, "hold": true, "wait": true,
	"yet": true, "later": true, "if": true, "unless": true, "until": true, "maybe": true, "whether": true,
}

// questionLeads open a question rather than a request.
var questionLeads = map[string]bool{
	"how": true, "what": true, "why": true, "when": true, "where": true, "which": true, "who": true,
	"can": true, "could": true, "would": true, "should": true, "shall": true, "will": true, "may": true,
	"do": true, "does": true, "did": true, "is": true, "are": true,
}

func words(text string) []string {
	lower := strings.ToLower(strings.ReplaceAll(text, "’", "'"))
	return strings.FieldsFunc(lower, func(r rune) bool {
		return r != '\'' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// IsStartRequest reports whether text explicitly and affirmatively asks to
// start a run: a clause opens with a start phrase ("start a run to ...",
// "new goal: ..."), and nothing negates, conditions or questions it. When it
// returns false the concierge asks instead of starting.
func IsStartRequest(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.Contains(text, "?") {
		return false
	}
	all := words(text)
	if len(all) == 0 || questionLeads[all[0]] {
		return false
	}
	for _, w := range all {
		if doubtWords[w] {
			return false
		}
	}
	for _, clause := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return strings.ContainsRune(".!;:\n", r) }) {
		c := strings.Join(words(clause), " ")
		for trimmed := true; trimmed; {
			trimmed = false
			for _, lead := range politeLeads {
				if rest, ok := strings.CutPrefix(c, lead+" "); ok {
					c, trimmed = rest, true
				}
			}
		}
		for _, p := range startPhrases {
			if c == p || strings.HasPrefix(c, p+" ") {
				return true
			}
		}
	}
	return false
}

// confirmations are plain answers of yes.
var confirmations = map[string]bool{
	"yes": true, "y": true, "yep": true, "yeah": true, "sure": true, "confirm": true, "confirmed": true,
	"go ahead": true, "do it": true, "start it": true, "yes please": true, "please do": true,
	"yes start it": true, "yes go ahead": true, "yes do it": true, "ok start it": true, "okay start it": true,
}

// IsConfirmation reports whether text is a plain yes, answering a question
// such as "Shall I start a run for ...?".
func IsConfirmation(text string) bool {
	return confirmations[strings.Join(words(text), " ")]
}

//nolint:govet // message payload grouping
type pollMsg struct{}

//nolint:govet // message payload grouping
type snapshotMsg struct {
	snapshot Snapshot
	err      error
}

//nolint:govet // message payload grouping
type routeMsg struct {
	text     string
	decision router.Decision
	err      error
}

//nolint:govet // message payload grouping
type commandMsg struct {
	text string
	err  error
}

//nolint:govet // message payload grouping
type streamMsg struct {
	text string
	err  error
	done bool
	ch   <-chan streamMsg
}

func (m Model) refresh() tea.Cmd {
	if m.Backend == nil {
		return nil
	}
	return func() tea.Msg { s, e := m.Backend.Snapshot(m.ctx); return snapshotMsg{s, e} }
}
func (m Model) Init() tea.Cmd {
	if m.poll {
		return tea.Batch(m.refresh(), poll())
	}
	return m.refresh()
}
func poll() tea.Cmd { return tea.Tick(time.Second, func(time.Time) tea.Msg { return pollMsg{} }) }
func receive(ctx context.Context, ch <-chan streamMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case x := <-ch:
			x.ch = ch
			return x
		case <-ctx.Done():
			return streamMsg{done: true, err: context.Canceled, ch: ch}
		}
	}
}
func (m Model) route(text string) tea.Cmd {
	return func() tea.Msg {
		if IsNewGoal(text) {
			return routeMsg{text: text, decision: router.Decision{Handler: router.Concierge, Confidence: 1, Backend: "goal-detection"}}
		}
		if m.router == nil {
			return routeMsg{text: text, decision: router.Decision{Handler: router.Concierge, Confidence: 1}}
		}
		input := text
		if m.Backend != nil {
			s, err := m.Backend.Snapshot(m.ctx)
			if err != nil {
				return routeMsg{text: text, err: err}
			}
			if len(s.Questions) > 0 {
				input = router.FormatInput(input, strings.Join(s.Questions, "\n"))
			}
		}
		d, e := m.router.Route(m.ctx, input)
		return routeMsg{text, d, e}
	}
}
func (m Model) dispatch(d router.Decision, text string) tea.Cmd {
	ch := make(chan streamMsg)
	go func() {
		emit := func(s string) {
			select {
			case ch <- streamMsg{text: s}:
			case <-m.ctx.Done():
			}
		}
		var err error
		if m.Dispatch != nil {
			err = m.Dispatch(m.ctx, d, text, emit)
		} else if d.Handler == router.Concierge && m.ask != nil {
			var s string
			s, err = m.ask(m.ctx, text)
			emit(s)
		} else {
			err = fmt.Errorf("no handler for %s", d.Handler)
		}
		select {
		case ch <- streamMsg{done: true, err: err}:
		case <-m.ctx.Done():
		}
	}()
	return receive(m.ctx, ch)
}
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case pollMsg:
		return m, tea.Batch(m.refresh(), poll())
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
	case snapshotMsg:
		if x.err != nil {
			m.lines = append(m.lines, "error: "+x.err.Error())
		} else {
			m.status = x.snapshot.Status
			m.questions = x.snapshot.Questions
		}
	case commandMsg:
		m.busy = false
		if x.err != nil {
			m.lines = append(m.lines, "error: "+x.err.Error())
		} else {
			m.lines = append(m.lines, x.text)
		}
		return m, m.refresh()
	case routeMsg:
		if x.err != nil {
			m.busy = false
			m.lines = append(m.lines, "error: "+x.err.Error())
			return m, nil
		}
		echo := "→ " + string(x.decision.Handler) + ": " + x.text
		if x.decision.Fallback {
			echo += " (via fallback)"
		}
		m.lines = append(m.lines, echo, "orca> ")
		m.streamLine = len(m.lines) - 1
		return m, m.dispatch(x.decision, x.text)
	case streamMsg:
		if x.text != "" {
			m.lines[m.streamLine] += x.text
		}
		if x.done {
			m.busy = false
			if x.err != nil {
				m.lines = append(m.lines, "error: "+x.err.Error())
			}
			return m, m.refresh()
		}
		return m, receive(m.ctx, x.ch)
	case tea.KeyPressMsg:
		if (x.Code == 'c' && x.Mod == tea.ModCtrl) || x.Code == tea.KeyEscape {
			return m, tea.Quit
		}
		if x.Code == tea.KeyBackspace {
			if m.input != "" {
				_, n := utf8.DecodeLastRuneInString(m.input)
				m.input = m.input[:len(m.input)-n]
			}
			return m, nil
		}
		if x.Code != tea.KeyEnter {
			m.input += x.Text
			return m, nil
		}
		if m.busy {
			return m, nil
		}
		text := strings.TrimSpace(m.input)
		m.input = ""
		if text == "" {
			return m, nil
		}
		switch strings.ToLower(text) {
		case "exit", "quit", "/exit", "/quit":
			return m, tea.Quit
		}
		m.lines = append(m.lines, "you> "+text)
		m.busy = true
		if strings.HasPrefix(text, "/") {
			return m, func() tea.Msg {
				if m.Backend == nil {
					return commandMsg{err: fmt.Errorf("no run attached")}
				}
				s, e := m.Backend.Command(m.ctx, text)
				return commandMsg{s, e}
			}
		}
		return m, m.route(text)
	case tea.PasteMsg:
		if !m.busy {
			m.input += x.Content
		}
		return m, nil
	}
	return m, nil
}
func (m Model) View() tea.View {
	runID := m.runID
	if backend, ok := m.Backend.(*StoreBackend); ok {
		runID = cmp.Or(backend.RunID(), runID)
	}
	lines := m.lines
	if m.height > 5 && len(lines) > m.height-5 {
		lines = lines[len(lines)-(m.height-5):]
	}
	questions := ""
	for _, q := range m.questions {
		questions += "\nowner question> " + q
	}
	return tea.NewView("orca chat — run " + runID + "\n" + strings.Join(lines, "\n") + questions + "\n\n" + m.status + "\n> " + m.input)
}
func isTerminal(in io.Reader) bool { f, ok := in.(*os.File); return ok && term.IsTerminal(int(f.Fd())) }
func Run(ctx context.Context, in io.Reader, out io.Writer, m Model) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.ctx = ctx
	if isTerminal(in) {
		m.poll = true
		_, err := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx)).Run()
		return err
	}
	// Scripts use the same update/command path as the terminal.
	if cmd := m.Init(); cmd != nil {
		next, _ := m.Update(cmd())
		m = next.(Model)
	}
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		text := strings.TrimSpace(scanner.Text())
		switch strings.ToLower(text) {
		case "exit", "quit", "/exit", "/quit":
			return nil
		}
		before := len(m.lines)
		m.input = text
		next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = next.(Model)
		for cmd != nil {
			msg := cmd()
			next, cmd = m.Update(msg)
			m = next.(Model)
		}
		for _, s := range m.lines[before:] {
			if _, err := fmt.Fprintln(out, s); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
